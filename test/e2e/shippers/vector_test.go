//go:build e2e

package shippers

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestE2E_Shipper_Vector(t *testing.T) {
	for _, compression := range []string{"none", "zstd"} {
		t.Run(compression, func(t *testing.T) {
			rig := StartLynxDB(t)
			fixture := writeFixture(t, 100)
			config := fmt.Sprintf(`
sources:
  fixture:
    type: file
    include: ["/var/log/fixture.log"]
    read_from: beginning
sinks:
  lynxdb:
    type: elasticsearch
    inputs: [fixture]
    endpoints: ["http://host.docker.internal:%d"]
    api_version: v8
    mode: bulk
    bulk:
      index: test-vector
    compression: %s
    healthcheck:
      enabled: true
`, rig.ESPort, compression)

			ctr := runContainer(t, testcontainers.ContainerRequest{
				Image:      "timberio/vector:0.40.0-alpine",
				Cmd:        []string{"--config", "/etc/vector/vector.yaml"},
				WaitingFor: wait.ForLog("Vector has started").WithStartupTimeout(60 * time.Second),
				Files: []testcontainers.ContainerFile{
					containerFile(fixture, "/var/log/fixture.log"),
					{Reader: strings.NewReader(config), ContainerFilePath: "/etc/vector/vector.yaml", FileMode: 0o644},
				},
			})

			waitForSourceCount(t, rig, "test-vector", 100)
			assertNoShipperErrors(t, containerLogs(t, ctr))
		})
	}
}
