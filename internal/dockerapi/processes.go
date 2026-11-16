package dockerapi

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/zedensix/homerun/internal/hoststats"
)

// HostProcess is a hoststats.Process with, for processes in a container, the
// name, image and homerun labels of that container, so the app can tell whose
// it is even when the container couldn't be sampled or is already gone.
type HostProcess struct {
	hoststats.Process
	ContainerImage string            `json:"containerImage,omitempty"`
	ContainerName  string            `json:"containerName,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// HostProcesses reads every process on the host from a helper container in the
// host's PID namespace: the worker runs in a container of its own, whose /proc
// only holds its own processes. The helper's own processes count as a
// container's, like every other. A container's processes carry its name,
// image and labels, from a listing that includes stopped containers so one
// that exited during the reading is still named.
func (c *Client) HostProcesses(ctx context.Context, image string) ([]HostProcess, error) {
	result, err := c.RunHelper(ctx, HelperConfig{
		Cmd:             []string{"sh", "-c", hoststats.ProcessScript},
		Image:           image,
		NetworkDisabled: true,
		PidMode:         "host",
	})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("reading the host's processes exited %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	parsed := hoststats.ParseProcesses(result.Stdout)
	containers := map[string]container.Summary{}
	if listed, err := c.listContainers(ctx, true, ""); err == nil {
		for _, entry := range listed {
			containers[entry.ID[:min(12, len(entry.ID))]] = entry
		}
	}
	processes := make([]HostProcess, 0, len(parsed))
	for _, process := range parsed {
		annotated := HostProcess{Process: process}
		if entry, ok := containers[process.Group]; ok && process.Container {
			annotated.ContainerImage = entry.Image
			annotated.ContainerName = containerName(entry)
			annotated.Labels = homerunLabels(entry.Labels)
		}
		processes = append(processes, annotated)
	}
	return processes, nil
}
