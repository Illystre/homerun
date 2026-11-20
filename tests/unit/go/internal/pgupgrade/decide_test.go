package pgupgrade_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zedensix/homerun/internal/pgupgrade"
)

func TestDecide(t *testing.T) {
	root := pgupgrade.Volume{Source: "db-data", Target: "/var/lib/postgresql"}
	legacy := pgupgrade.Volume{Source: "db-data", Target: "/var/lib/postgresql/data"}
	cases := []struct {
		name    string
		version string
		pgdata  string
		found   []pgupgrade.Found
		want    *pgupgrade.Plan
	}{
		{
			name:    "18 to 19 in the 18+ layout",
			version: "19",
			pgdata:  "/var/lib/postgresql/19/docker",
			found: []pgupgrade.Found{{Volume: root, Clusters: []pgupgrade.Cluster{
				{Dir: "/var/lib/postgresql/18/docker", Version: "18"},
			}}},
			want: &pgupgrade.Plan{NewDir: "/var/lib/postgresql/19/docker", NewVersion: "19", OldDir: "/var/lib/postgresql/18/docker", OldVersion: "18", Volume: root},
		},
		{
			name:    "already upgraded",
			version: "19",
			pgdata:  "/var/lib/postgresql/19/docker",
			found: []pgupgrade.Found{{Volume: root, Clusters: []pgupgrade.Cluster{
				{Dir: "/var/lib/postgresql/18/docker", Version: "18"},
				{Dir: "/var/lib/postgresql/19/docker", Version: "19"},
			}}},
		},
		{
			name:    "the newest older cluster wins",
			version: "19",
			pgdata:  "/var/lib/postgresql/19/docker",
			found: []pgupgrade.Found{{Volume: root, Clusters: []pgupgrade.Cluster{
				{Dir: "/var/lib/postgresql", Version: "9.6"},
				{Dir: "/var/lib/postgresql/18/docker", Version: "18"},
				{Dir: "/var/lib/postgresql/17/docker", Version: "17"},
			}}},
			want: &pgupgrade.Plan{NewDir: "/var/lib/postgresql/19/docker", NewVersion: "19", OldDir: "/var/lib/postgresql/18/docker", OldVersion: "18", Volume: root},
		},
		{
			name:    "a pre-18 cluster at the volume root",
			version: "18",
			pgdata:  "/var/lib/postgresql/18/docker",
			found: []pgupgrade.Found{{Volume: root, Clusters: []pgupgrade.Cluster{
				{Dir: "/var/lib/postgresql", Version: "17"},
			}}},
			want: &pgupgrade.Plan{NewDir: "/var/lib/postgresql/18/docker", NewVersion: "18", OldDir: "/var/lib/postgresql", OldVersion: "17", Volume: root},
		},
		{
			name:    "same directory before 18",
			version: "17",
			pgdata:  "/var/lib/postgresql/data",
			found: []pgupgrade.Found{{Volume: legacy, Clusters: []pgupgrade.Cluster{
				{Dir: "/var/lib/postgresql/data", Version: "16"},
			}}},
			want: &pgupgrade.Plan{NewDir: "/var/lib/postgresql/data", NewVersion: "17", OldDir: "/var/lib/postgresql/data", OldVersion: "16", Volume: legacy},
		},
		{
			name:    "a downgrade is left to Postgres",
			version: "16",
			pgdata:  "/var/lib/postgresql/data",
			found: []pgupgrade.Found{{Volume: legacy, Clusters: []pgupgrade.Cluster{
				{Dir: "/var/lib/postgresql/data", Version: "17"},
			}}},
		},
		{
			name:    "an empty volume",
			version: "19",
			pgdata:  "/var/lib/postgresql/19/docker",
			found:   []pgupgrade.Found{{Volume: root}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pgupgrade.Decide(tc.version, tc.pgdata, tc.found)
			if err != nil {
				t.Fatalf("Decide: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Decide = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDecideRefusesDataOutsideEveryVolume(t *testing.T) {
	legacy := pgupgrade.Volume{Source: "db-data", Target: "/var/lib/postgresql/data"}
	_, err := pgupgrade.Decide("18", "/var/lib/postgresql/18/docker", []pgupgrade.Found{{Volume: legacy, Clusters: []pgupgrade.Cluster{
		{Dir: "/var/lib/postgresql/data", Version: "17"},
	}}})
	if err == nil || !strings.Contains(err.Error(), "/var/lib/postgresql") {
		t.Fatalf("Decide error = %v, want advice to remount", err)
	}
}

func TestSameVariant(t *testing.T) {
	cases := map[string]string{
		"postgres:19.0-alpine":                       "postgres:18-alpine",
		"postgres:19":                                "postgres:18",
		"postgres:19.0-alpine@sha256:abc":            "postgres:18-alpine",
		"pgvector/pgvector:pg19-trixie":              "pgvector/pgvector:pg18-trixie",
		"registry.local:5000/postgis/postgis:19-3.6": "registry.local:5000/postgis/postgis:18-3.6",
		"postgres":        "postgres:18",
		"postgres:alpine": "postgres:18",
	}
	for image, want := range cases {
		if got := pgupgrade.SameVariant(image, "18"); got != want {
			t.Errorf("SameVariant(%q) = %q, want %q", image, got, want)
		}
	}
}
