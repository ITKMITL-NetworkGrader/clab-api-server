package clab

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
)

// NTG-114: containerlab's destroy of a lab whose containers carry several
// topo-file labels re-creates the lab's management network with nothing on it.

func orphanTestClient(t *testing.T) *dockerclient.Client {
	t.Helper()
	client, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("Docker client unavailable: %v", err)
	}
	if _, err := client.Ping(context.Background()); err != nil {
		t.Skipf("Docker daemon unavailable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func orphanTestNetwork(t *testing.T, client *dockerclient.Client, labels map[string]string) string {
	t.Helper()
	lab := fmt.Sprintf("ntg114-test-%d", time.Now().UnixNano())
	created, err := client.NetworkCreate(context.Background(), "clab-"+lab, network.CreateOptions{Labels: labels})
	if err != nil {
		t.Fatalf("create test network: %v", err)
	}
	t.Cleanup(func() { _ = client.NetworkRemove(context.Background(), created.ID) })
	return lab
}

func TestRemoveOrphanManagementNetworkRemovesAnEmptyContainerlabNetwork(t *testing.T) {
	client := orphanTestClient(t)
	lab := orphanTestNetwork(t, client, map[string]string{"containerlab": ""})

	removed, err := NewService().RemoveOrphanManagementNetwork(context.Background(), lab)
	if err != nil || !removed {
		t.Fatalf("removed = %v, err = %v; want true, nil", removed, err)
	}
	if exists, _ := NewService().ManagementNetworkExists(context.Background(), "clab-"+lab); exists {
		t.Fatal("network still exists after removal")
	}
}

func TestRemoveOrphanManagementNetworkKeepsANetworkThatStillHasContainers(t *testing.T) {
	client := orphanTestClient(t)
	ctx := context.Background()
	lab := orphanTestNetwork(t, client, map[string]string{"containerlab": ""})
	created, err := client.ContainerCreate(ctx,
		&container.Config{Image: "alpine:3.20", Cmd: []string{"sleep", "60"}},
		&container.HostConfig{NetworkMode: container.NetworkMode("clab-" + lab)}, nil, nil, "")
	if err != nil {
		t.Skipf("alpine:3.20 unavailable: %v", err)
	}
	t.Cleanup(func() { _ = client.ContainerRemove(ctx, created.ID, container.RemoveOptions{Force: true}) })
	if err := client.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start container: %v", err)
	}

	removed, err := NewService().RemoveOrphanManagementNetwork(ctx, lab)
	if removed || !errors.Is(err, ErrManagementNetworkInUse) {
		t.Fatalf("removed = %v, err = %v; want false, ErrManagementNetworkInUse", removed, err)
	}
}

func TestRemoveOrphanManagementNetworkIgnoresANetworkWithoutTheContainerlabLabel(t *testing.T) {
	client := orphanTestClient(t)
	lab := orphanTestNetwork(t, client, nil)

	removed, err := NewService().RemoveOrphanManagementNetwork(context.Background(), lab)
	if err != nil || removed {
		t.Fatalf("removed = %v, err = %v; want false, nil", removed, err)
	}
	if exists, _ := NewService().ManagementNetworkExists(context.Background(), "clab-"+lab); !exists {
		t.Fatal("unlabelled network was removed")
	}
}

func TestRemoveOrphanManagementNetworkAbsentIsNotAnError(t *testing.T) {
	orphanTestClient(t)
	removed, err := NewService().RemoveOrphanManagementNetwork(context.Background(), fmt.Sprintf("ntg114-absent-%d", time.Now().UnixNano()))
	if err != nil || removed {
		t.Fatalf("removed = %v, err = %v; want false, nil", removed, err)
	}
}

func TestRemoveOrphanManagementNetworkNeverTouchesTheSharedClabNetwork(t *testing.T) {
	if _, err := NewService().RemoveOrphanManagementNetwork(context.Background(), ""); err == nil {
		t.Fatal("an empty lab name (which would target the shared 'clab' network) was accepted")
	}
}
