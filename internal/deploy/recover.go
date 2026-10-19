package deploy

import (
	"context"
	"fmt"
)

// InterruptedMessage explains an operation a control-plane restart cut off.
const InterruptedMessage = "interrupted: the control plane restarted while this was running"

// RecoverInterrupted settles the deployments a control-plane restart cut off.
//
// A deployment cleans up after itself when it fails, but not when the process
// running it stops: its candidate containers stayed up beside the live ones,
// sharing their network alias, a private service it had stopped to make room
// stayed stopped, and the deployment read as running forever. This runs at
// startup, before any operation can start, and puts each affected project
// back on the containers its active release records.
func (d *Deployer) RecoverInterrupted(ctx context.Context, log func(string, ...any)) error {
	interrupted, err := d.Store.InterruptedDeployments(ctx)
	if err != nil || len(interrupted) == 0 {
		return err
	}
	containers, err := d.Docker.Containers(ctx, true)
	if err != nil {
		return err
	}
	for _, item := range interrupted {
		active, err := d.Store.ActiveRuntimes(ctx, item.ProjectID)
		if err != nil {
			return err
		}
		activeIDs := map[string]bool{}
		for _, runtime := range active {
			activeIDs[runtime.DockerID] = true
		}
		for _, container := range containers {
			// Only release containers carry a release label; helpers and
			// anything Asgard did not create are left alone.
			if container.ProjectID != item.ProjectID || container.ReleaseID == "" || activeIDs[container.ID] {
				continue
			}
			if err := d.Docker.Remove(ctx, container.ID, false); err != nil {
				log("could not remove a container an interrupted deployment left", "container", container.Name, "error", err)
				continue
			}
			log("removed a container an interrupted deployment left", "container", container.Name)
		}
		for _, container := range containers {
			if activeIDs[container.ID] && container.State != "running" && container.State != "restarting" {
				if err := d.Docker.Action(ctx, container.ID, "start"); err != nil {
					log("could not restart an active container an interrupted deployment stopped", "container", container.Name, "error", err)
				} else {
					log("restarted an active container an interrupted deployment stopped", "container", container.Name)
				}
			}
		}
		status, message := "failed", InterruptedMessage
		if item.ReleaseID != "" {
			if live, err := d.Store.ReleaseServesTraffic(ctx, item.ReleaseID); err == nil && live {
				// The switch had committed: this release is the one running,
				// and its predecessor's containers were just removed above.
				status, message = "succeeded", InterruptedMessage+" after the release went live; the previous release was retired at startup"
			}
			if err := d.Store.FinishRelease(ctx, item.ReleaseID, item.ID, status, message); err != nil {
				return fmt.Errorf("settle interrupted release: %w", err)
			}
		}
		_ = d.Store.LogOperation(ctx, item.OperationID, "error", message)
		if err := d.Store.CompleteOperation(ctx, item.OperationID, status, message); err != nil {
			return err
		}
	}
	return nil
}
