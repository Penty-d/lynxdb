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

func TestE2E_Shipper_FluentBit(t *testing.T) {
	rig := StartLynxDB(t)
	fixture := writeFixture(t, 100)
	config := fmt.Sprintf(`
[INPUT]
    Name  tail
    Path  /var/log/fixture.log
    Tag   fixture
    Read_From_Head true

[OUTPUT]
    Name  es
    Match *
    Host  host.docker.internal
    Port  %d
    Suppress_Type_Name On
    Logstash_Format Off
    Index test-fluentbit
`, rig.ESPort)

	ctr := runContainer(t, testcontainers.ContainerRequest{
		Image:      "cr.fluentbit.io/fluent/fluent-bit:3.1",
		Cmd:        []string{"-c", "/fluent-bit/etc/fluent-bit.conf"},
		WaitingFor: wait.ForLog("[output:es:").WithStartupTimeout(60 * time.Second),
		Files: []testcontainers.ContainerFile{
			containerFile(fixture, "/var/log/fixture.log"),
			{Reader: strings.NewReader(config), ContainerFilePath: "/fluent-bit/etc/fluent-bit.conf", FileMode: 0o644},
		},
	})

	waitForSourceCount(t, rig, "test-fluentbit", 100)
	assertNoShipperErrors(t, containerLogs(t, ctr))
}
