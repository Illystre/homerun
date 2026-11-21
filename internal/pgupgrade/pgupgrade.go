// Package pgupgrade moves a Postgres data directory across a major version
// when a service's image switches majors. The official image only runs a
// cluster made by its own major, and from 18 on refuses to start next to an
// older cluster, so swapping the tag alone leaves the database down. The
// upgrade dumps the old cluster with the old major's binaries and restores it
// into a fresh cluster made by the new image, at the directory the new image
// reads, inside the same volume. The old cluster is kept beside it.
package pgupgrade

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zedensix/homerun/internal/dockerapi"
)

const (
	workDir     = ".homerun-pg-upgrade"
	stepTimeout = 6 * time.Hour
	probeTime   = 2 * time.Minute
	errorLines  = 20
)

// Volume is a writable mount of the service: its volume name or host path and
// where it's mounted in the container.
type Volume struct {
	Source string `json:"source"`
	Target string `json:"target"`
}

// Request is the service about to run Image: its environment (KEY=VALUE),
// its writable mounts, and the images it ran before, newest first, which the
// old cluster is dumped with when one of them is of the old major.
type Request struct {
	Env      []string `json:"env"`
	Image    string   `json:"image"`
	Previous []string `json:"previous"`
	Volumes  []Volume `json:"volumes"`
}

// Cluster is a Postgres data directory found in a volume, with the major
// version its PG_VERSION file names.
type Cluster struct {
	Dir     string
	Version string
}

// Found is the clusters one volume holds.
type Found struct {
	Clusters []Cluster
	Volume   Volume
}

// Plan is one upgrade to run: the cluster in OldDir, made by OldVersion,
// restored into NewDir for NewVersion, both inside Volume.
type Plan struct {
	NewDir     string
	NewVersion string
	OldDir     string
	OldVersion string
	Volume     Volume
}

// Find inspects request.Image and the service's volumes and returns the
// upgrade its data needs, or nil when the image isn't Postgres, no volume
// holds an older cluster, or the new major's cluster is already there. It
// fails when the image can't be inspected, a volume can't be read, or the old
// cluster sits in a volume the new major's data directory isn't in.
func Find(ctx context.Context, docker *dockerapi.Client, request Request) (*Plan, error) {
	inspected, err := docker.InspectImage(ctx, request.Image)
	if err != nil {
		return nil, err
	}
	version := envValue(inspected.Config.Env, "PG_MAJOR")
	pgdata := envValue(request.Env, "PGDATA")
	if pgdata == "" {
		pgdata = envValue(inspected.Config.Env, "PGDATA")
	}
	if version == "" || pgdata == "" || len(request.Volumes) == 0 {
		return nil, nil
	}
	found := make([]Found, 0, len(request.Volumes))
	for _, volume := range request.Volumes {
		clusters, err := probe(ctx, docker, request.Image, volume)
		if err != nil {
			return nil, err
		}
		found = append(found, Found{Clusters: clusters, Volume: volume})
	}
	return Decide(version, path.Clean(pgdata), found)
}

// Decide picks the upgrade version's image needs, its data directory being
// pgdata: none when the volume holding pgdata already has a cluster there of
// version or newer, or no older cluster at all; otherwise the newest older
// cluster in that volume. It fails when an older cluster sits in a volume
// that doesn't hold pgdata, since the new major would then start an empty
// database outside every volume.
func Decide(version, pgdata string, found []Found) (*Plan, error) {
	wanted, ok := parseVersion(version)
	if !ok {
		return nil, nil
	}
	for _, entry := range found {
		if within(pgdata, entry.Volume.Target) {
			return decideIn(entry, version, wanted, pgdata), nil
		}
	}
	for _, entry := range found {
		if old := newestOlder(entry.Clusters, wanted); old != nil {
			return nil, fmt.Errorf(
				"%s holds a Postgres %s cluster in %s, but Postgres %s keeps its data in %s, which no volume covers: mount %s at a directory that contains %s (for the official image, /var/lib/postgresql) and deploy again to upgrade it",
				entry.Volume.Source, old.Version, old.Dir, version, pgdata, entry.Volume.Source, pgdata,
			)
		}
	}
	return nil, nil
}

// Run dumps the plan's old cluster with an image of its major and restores it
// into a new cluster made by request.Image, streaming every step's output to
// log line by line. The new cluster takes the old one's pg_hba.conf and
// pg_ident.conf; postgresql.conf and ALTER SYSTEM settings start from the new
// image's defaults. The old cluster is kept: beside the new one when they
// live in different directories, else moved into a homerun-pg<version>-<time>
// directory inside it. Nothing of the old cluster is moved until the restore
// succeeded. The service's own containers must be stopped first. It fails
// when no image of the old major can be found or pulled, or a step fails,
// with that step's last output.
func (p *Plan) Run(ctx context.Context, docker *dockerapi.Client, request Request, log func(string)) error {
	oldImage, err := oldImage(ctx, docker, request.Previous, request.Image, p.OldVersion, log)
	if err != nil {
		return err
	}
	env := []string{
		"OLD=" + p.OldDir,
		"NEW=" + p.NewDir,
		"OLD_VERSION=" + p.OldVersion,
		"WORK=" + path.Join(p.Volume.Target, workDir),
		"SUPERUSER=" + superuser(request.Env),
		"POSTGRES_INITDB_ARGS=" + envValue(request.Env, "POSTGRES_INITDB_ARGS"),
	}
	log(fmt.Sprintf("Upgrading the Postgres %s cluster in %s to %s, into %s", p.OldVersion, p.OldDir, p.NewVersion, p.NewDir))
	log(fmt.Sprintf("Dumping it with %s", oldImage))
	if err := step(ctx, docker, oldImage, dumpScript, env, p.Volume, log); err != nil {
		return fmt.Errorf("couldn't dump the Postgres %s cluster: %w", p.OldVersion, err)
	}
	log(fmt.Sprintf("Restoring it with %s", request.Image))
	if err := step(ctx, docker, request.Image, restoreScript, env, p.Volume, log); err != nil {
		return fmt.Errorf("couldn't restore into Postgres %s: %w", p.NewVersion, err)
	}
	if p.OldDir == p.NewDir {
		log(fmt.Sprintf("Upgraded to Postgres %s. The %s cluster is kept in %s/homerun-pg%s-*, delete it once %s runs fine", p.NewVersion, p.OldVersion, p.OldDir, p.OldVersion, p.NewVersion))
	} else {
		log(fmt.Sprintf("Upgraded to Postgres %s. The %s cluster is kept in %s, delete it once %s runs fine", p.NewVersion, p.OldVersion, p.OldDir, p.NewVersion))
	}
	return nil
}

// decideIn is Decide for the volume that holds pgdata.
func decideIn(entry Found, version string, wanted float64, pgdata string) *Plan {
	for _, cluster := range entry.Clusters {
		if cluster.Dir != pgdata {
			continue
		}
		have, ok := parseVersion(cluster.Version)
		if !ok || have >= wanted {
			return nil
		}
	}
	old := newestOlder(entry.Clusters, wanted)
	if old == nil {
		return nil
	}
	return &Plan{NewDir: pgdata, NewVersion: version, OldDir: old.Dir, OldVersion: old.Version, Volume: entry.Volume}
}

// newestOlder is the cluster of the highest version below wanted, or nil.
func newestOlder(clusters []Cluster, wanted float64) *Cluster {
	var newest *Cluster
	newestVersion := -1.0
	for index := range clusters {
		have, ok := parseVersion(clusters[index].Version)
		if ok && have < wanted && have > newestVersion {
			newest, newestVersion = &clusters[index], have
		}
	}
	return newest
}

// within reports whether dir is target or inside it.
func within(dir, target string) bool {
	target = path.Clean(target)
	return dir == target || strings.HasPrefix(dir, strings.TrimSuffix(target, "/")+"/")
}

// parseVersion reads a PG_VERSION or PG_MAJOR value, "9.6" and "18" alike.
func parseVersion(version string) (float64, bool) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(version), 64)
	return parsed, err == nil
}

// envValue is the last value env sets key to, or "".
func envValue(env []string, key string) string {
	value := ""
	for _, entry := range env {
		if name, rest, ok := strings.Cut(entry, "="); ok && name == key {
			value = rest
		}
	}
	return value
}

// superuser is the role the official image bootstraps the cluster with.
func superuser(env []string) string {
	if user := envValue(env, "POSTGRES_USER"); user != "" {
		return user
	}
	return "postgres"
}

// probe lists the clusters volume holds up to three levels down, read by a
// throwaway container of image with the volume at its own target.
func probe(ctx context.Context, docker *dockerapi.Client, image string, volume Volume) ([]Cluster, error) {
	result, err := docker.RunOneOff(ctx, dockerapi.OneOffConfig{
		Binds:       []string{volume.Source + ":" + volume.Target},
		Cmd:         []string{"probe", volume.Target},
		Entrypoint:  []string{"sh", "-c", probeScript},
		Image:       image,
		NetworkMode: "none",
		Timeout:     probeTime,
		User:        "root",
	})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 || result.TimedOut {
		return nil, fmt.Errorf("couldn't look for a Postgres cluster in %s: %s", volume.Source, lastLines(result))
	}
	var clusters []Cluster
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		dir, version, ok := strings.Cut(line, "\t")
		if ok && !strings.Contains(dir, "/"+workDir) {
			clusters = append(clusters, Cluster{Dir: path.Clean(dir), Version: strings.TrimSpace(version)})
		}
	}
	return clusters, nil
}

// oldImage is the first of previous on this daemon whose PG_MAJOR is
// version, else the new image's own repository and variant at version (see
// sameVariant), pulled when missing.
func oldImage(ctx context.Context, docker *dockerapi.Client, previous []string, image, version string, log func(string)) (string, error) {
	for _, candidate := range previous {
		inspected, err := docker.InspectImage(ctx, candidate)
		if err == nil && envValue(inspected.Config.Env, "PG_MAJOR") == version {
			return candidate, nil
		}
	}
	fallback := SameVariant(image, version)
	exists, err := docker.ImageExists(ctx, fallback)
	if err != nil {
		return "", err
	}
	if !exists {
		log(fmt.Sprintf("None of the service's earlier images is Postgres %s, pulling %s to dump with", version, fallback))
		if err := docker.PullImage(ctx, fallback, nil, nil); err != nil {
			return "", fmt.Errorf("couldn't pull %s to dump the Postgres %s cluster with: %w", fallback, version, err)
		}
	}
	return fallback, nil
}

// SameVariant is image with the first version in its tag swapped for
// version, so the old cluster is dumped by the same build (Alpine or Debian,
// the same extensions) it was made by: postgres:19.0-alpine at 18 is
// postgres:18-alpine, pgvector/pgvector:pg18 is pgvector/pgvector:pg17. A tag
// with no version falls back to the official postgres:<version>.
func SameVariant(image, version string) string {
	name, _, _ := strings.Cut(image, "@")
	tag := "latest"
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		name, tag = name[:colon], name[colon+1:]
	}
	found := leadingVersion.FindStringSubmatchIndex(tag)
	if found == nil {
		return "postgres:" + version
	}
	return name + ":" + tag[:found[2]] + version + tag[found[3]:]
}

var leadingVersion = regexp.MustCompile(`^\D*?(\d+(?:\.\d+)*)`)

// step runs script as root in a throwaway container of image with the
// volume mounted and no network, streaming its output to log.
func step(ctx context.Context, docker *dockerapi.Client, image, script string, env []string, volume Volume, log func(string)) error {
	output := &lineWriter{emit: log}
	result, err := docker.RunOneOff(ctx, dockerapi.OneOffConfig{
		Binds:       []string{volume.Source + ":" + volume.Target},
		Entrypoint:  []string{"sh", "-c", script},
		Env:         env,
		Image:       image,
		NetworkMode: "none",
		OnOutput:    output.write,
		Timeout:     stepTimeout,
		User:        "root",
	})
	output.flush()
	if err != nil {
		return err
	}
	if result.TimedOut {
		return errors.New("timed out after " + stepTimeout.String())
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("exited %d: %s", result.ExitCode, lastLines(result))
	}
	return nil
}

// lastLines is the tail of a one-off's stderr, or of its stdout when
// stderr is empty.
func lastLines(result dockerapi.OneOffResult) string {
	text := strings.TrimSpace(string(result.Stderr))
	if text == "" {
		text = strings.TrimSpace(string(result.Stdout))
	}
	lines := strings.Split(text, "\n")
	return strings.Join(lines[max(0, len(lines)-errorLines):], "\n")
}

// lineWriter turns output chunks into whole lines.
type lineWriter struct {
	emit    func(string)
	pending string
}

// write buffers chunk and emits every line it completes.
func (w *lineWriter) write(chunk string) {
	w.pending += chunk
	for {
		end := strings.IndexByte(w.pending, '\n')
		if end < 0 {
			return
		}
		w.emit(strings.TrimRight(w.pending[:end], "\r"))
		w.pending = w.pending[end+1:]
	}
}

// flush emits what's left of an unterminated last line.
func (w *lineWriter) flush() {
	if strings.TrimSpace(w.pending) != "" {
		w.emit(w.pending)
	}
	w.pending = ""
}

const probeScript = `find "$1" -maxdepth 3 -name PG_VERSION -type f | while read -r file; do printf '%s\t%s\n' "$(dirname "$file")" "$(head -n 1 "$file")"; done`

const asPostgres = `as_postgres() { if command -v gosu >/dev/null 2>&1; then gosu postgres "$@"; else su-exec postgres "$@"; fi; }
uid=$(stat -c %u "$OLD")
gid=$(stat -c %g "$OLD")
sed -i "s/^postgres:\([^:]*\):[0-9]*:[0-9]*:/postgres:\1:$uid:$gid:/" /etc/passwd
sed -i "s/^postgres:\([^:]*\):[0-9]*:/postgres:\1:$gid:/" /etc/group
printf 'local all all trust\n' > /tmp/pg_hba.conf
chown postgres /tmp/pg_hba.conf
server_options="-c listen_addresses='' -c unix_socket_directories=/tmp -c hba_file=/tmp/pg_hba.conf"
`

const dumpScript = `set -eu
` + asPostgres + `
trap '[ "$?" -eq 0 ] || rm -rf "$WORK"' EXIT
rm -rf "$WORK"
mkdir -p "$WORK"
chown postgres:postgres "$WORK"
rm -f "$OLD/postmaster.pid"
as_postgres pg_ctl -D "$OLD" -w -t 3600 -l /tmp/postgres.log -o "$server_options" start || { cat /tmp/postgres.log >&2; exit 1; }
status=0
as_postgres pg_dumpall -h /tmp -U "$SUPERUSER" -f "$WORK/dump.sql" || status=$?
as_postgres pg_ctl -D "$OLD" -w -t 3600 -m fast stop
echo "Dumped $(du -h "$WORK/dump.sql" | cut -f1)"
exit "$status"
`

const restoreScript = `set -eu
` + asPostgres + `
trap 'rm -rf "$WORK"' EXIT
[ -s "$WORK/dump.sql" ] || { echo "The dump is missing" >&2; exit 1; }
NEXT="$WORK/next"
rm -rf "$NEXT"
mkdir -p "$NEXT"
chown postgres:postgres "$NEXT"
chmod 700 "$NEXT"
eval 'as_postgres initdb -D "$NEXT" --username="$SUPERUSER" '"$POSTGRES_INITDB_ARGS"
for file in pg_hba.conf pg_ident.conf; do
	if [ -f "$OLD/$file" ]; then
		cp "$OLD/$file" "$NEXT/$file"
		chown postgres:postgres "$NEXT/$file"
	fi
done
as_postgres pg_ctl -D "$NEXT" -w -t 3600 -l /tmp/postgres.log -o "$server_options" start || { cat /tmp/postgres.log >&2; exit 1; }
status=0
as_postgres psql -X -q -h /tmp -U "$SUPERUSER" -d postgres -f "$WORK/dump.sql" > /dev/null 2> "$WORK/restore.log" || status=$?
as_postgres pg_ctl -D "$NEXT" -w -t 3600 -m fast stop
if [ "$status" -ne 0 ] || grep 'ERROR:' "$WORK/restore.log" | grep -qv "role \"$SUPERUSER\" already exists"; then
	cat "$WORK/restore.log" >&2
	exit 1
fi
echo "Restored"
if [ "$OLD" = "$NEW" ]; then
	KEEP="$OLD/homerun-pg$OLD_VERSION-$(date +%Y%m%d%H%M%S)"
	mkdir "$KEEP"
	for entry in "$OLD"/*; do
		case "$entry" in "$OLD"/homerun-pg*) continue ;; esac
		mv "$entry" "$KEEP/"
	done
	mv "$NEXT"/* "$NEW/"
else
	if [ -e "$NEW" ]; then rmdir "$NEW"; fi
	mkdir -p "$(dirname "$NEW")"
	mv "$NEXT" "$NEW"
fi
`
