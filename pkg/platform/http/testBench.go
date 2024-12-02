//go:build linux
package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func (a *AgentClient) SendKtPID(_ context.Context, id uint64) error {
	a.logger.Debug("Inside SendKtPID of agent binary !!", zap.Uint64("clientID", id))
	ktPid := uint32(os.Getpid())

	time.Sleep(3 * time.Second)
	//Extracting the pid of the keployTest agent
	pid, err := GetPIDFromPort(8090)
	if err != nil {
		utils.LogError(a.logger, err, "failed to get the keployTest pid")
		return fmt.Errorf("error getting keployTest pid: %s", err.Error())
	}

	tb := models.TestBenchReq{
		KtclientID: id,
		KtPid:      ktPid,
		KaPid:      uint32(pid),
	}

	// Marshal the ktPid to send it to the server
	requestJSON, err := json.Marshal(tb)
	if err != nil {
		utils.LogError(a.logger, err, "failed to marshal request body for register client")
		return fmt.Errorf("error marshaling request body for register client: %s", err.Error())
	}

	// Send the ktPid to the server
	resp, err := a.client.Post("http://localhost:8086/agent/testbench", "application/json", bytes.NewBuffer(requestJSON))
	if err != nil {
		utils.LogError(a.logger, err, "failed to send register client request")
		return fmt.Errorf("error sending register client request: %s", err.Error())
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("failed to send keployTest Pid %s", resp.Status)
	}

	return nil
}

func GetPIDFromPort(port int) (int, error) {
	cmd := exec.Command("sudo", "lsof", "-i", fmt.Sprintf(":%d", port))
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return 0, fmt.Errorf("error executing lsof command: %v", err)
	}

	// Parse the output to find the PID
	lines := strings.Split(out.String(), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 1 && fields[1] != "PID" {
			pid, err := strconv.Atoi(fields[1])
			if err != nil {
				return 0, fmt.Errorf("error parsing PID: %v", err)
			}
			return pid, nil
		}
	}
	return 0, fmt.Errorf("no process found on port %d", port)
}
