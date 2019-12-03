/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cluster

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strings"
	"syscall"
	"time"

	"vitess.io/vitess/go/vt/log"
)

// VttabletProcess is a generic handle for a running vttablet .
// It can be spawned manually
type VttabletProcess struct {
	Name                        string
	Binary                      string
	FileToLogQueries            string
	TabletUID                   int
	TabletPath                  string
	Cell                        string
	Port                        int
	GrpcPort                    int
	PidFile                     string
	Shard                       string
	CommonArg                   VtctlProcess
	LogDir                      string
	TabletHostname              string
	Keyspace                    string
	TabletType                  string
	HealthCheckInterval         int
	BackupStorageImplementation string
	FileBackupStorageRoot       string
	ServiceMap                  string
	VtctldAddress               string
	Directory                   string
	VerifyURL                   string
	//Extra Args to be set before starting the vttablet process
	ExtraArgs []string

	proc *exec.Cmd
	exit chan error
}

// Setup starts vtctld process with required arguements
func (vttablet *VttabletProcess) Setup() (err error) {

	vttablet.proc = exec.Command(
		vttablet.Binary,
		"-topo_implementation", vttablet.CommonArg.TopoImplementation,
		"-topo_global_server_address", vttablet.CommonArg.TopoGlobalAddress,
		"-topo_global_root", vttablet.CommonArg.TopoGlobalRoot,
		"-log_queries_to_file", vttablet.FileToLogQueries,
		"-tablet-path", vttablet.TabletPath,
		"-port", fmt.Sprintf("%d", vttablet.Port),
		"-grpc_port", fmt.Sprintf("%d", vttablet.GrpcPort),
		"-pid_file", vttablet.PidFile,
		"-init_shard", vttablet.Shard,
		"-log_dir", vttablet.LogDir,
		"-tablet_hostname", vttablet.TabletHostname,
		"-init_keyspace", vttablet.Keyspace,
		"-init_tablet_type", vttablet.TabletType,
		"-health_check_interval", fmt.Sprintf("%ds", vttablet.HealthCheckInterval),
		"-enable_semi_sync",
		"-enable_replication_reporter",
		"-backup_storage_implementation", vttablet.BackupStorageImplementation,
		"-file_backup_storage_root", vttablet.FileBackupStorageRoot,
		"-restore_from_backup",
		"-service_map", vttablet.ServiceMap,
		"-vtctld_addr", vttablet.VtctldAddress,
	)
	vttablet.proc.Args = append(vttablet.proc.Args, vttablet.ExtraArgs...)

	vttablet.proc.Stderr = os.Stderr
	vttablet.proc.Stdout = os.Stdout

	vttablet.proc.Env = append(vttablet.proc.Env, os.Environ()...)

	log.Infof("%v %v", strings.Join(vttablet.proc.Args, " "))

	err = vttablet.proc.Start()
	if err != nil {
		return
	}

	vttablet.exit = make(chan error)
	go func() {
		vttablet.exit <- vttablet.proc.Wait()
	}()

	timeout := time.Now().Add(60 * time.Second)
	for time.Now().Before(timeout) {
		if vttablet.WaitForStatus("NOT_SERVING") {
			return nil
		}
		select {
		case err := <-vttablet.exit:
			return fmt.Errorf("process '%s' exited prematurely (err: %s)", vttablet.Name, err)
		default:
			time.Sleep(300 * time.Millisecond)
		}
	}

	return fmt.Errorf("process '%s' timed out after 60s (err: %s)", vttablet.Name, <-vttablet.exit)
}

// WaitForStatus function checks if vttablet process is up and running
func (vttablet *VttabletProcess) WaitForStatus(status string) bool {
	resp, err := http.Get(vttablet.VerifyURL)
	if err != nil {
		return false
	}
	if resp.StatusCode == 200 {
		resultMap := make(map[string]interface{})
		respByte, _ := ioutil.ReadAll(resp.Body)
		err := json.Unmarshal(respByte, &resultMap)
		if err != nil {
			panic(err)
		}
		return resultMap["TabletStateName"] == status
	}
	return false
}

// TearDown shuts down the running vttablet service
func (vttablet *VttabletProcess) TearDown() error {
	if vttablet.proc == nil {
		fmt.Printf("No process found for vttablet %d", vttablet.TabletUID)
	}
	if vttablet.proc == nil || vttablet.exit == nil {
		return nil
	}
	// Attempt graceful shutdown with SIGTERM first
	vttablet.proc.Process.Signal(syscall.SIGTERM)

	select {
	case <-vttablet.exit:
		vttablet.proc = nil
		return nil

	case <-time.After(10 * time.Second):
		vttablet.proc.Process.Kill()
		vttablet.proc = nil
		return <-vttablet.exit
	}
}

// VttabletProcessInstance returns a VttabletProcess handle for vttablet process
// configured with the given Config.
// The process must be manually started by calling setup()
func VttabletProcessInstance(port int, grpcPort int, tabletUID int, cell string, shard string, keyspace string, vtctldPort int, tabletType string, topoPort int, hostname string, tmpDirectory string, extraArgs []string) *VttabletProcess {
	vtctl := VtctlProcessInstance(topoPort, hostname)
	vttablet := &VttabletProcess{
		Name:                        "vttablet",
		Binary:                      "vttablet",
		FileToLogQueries:            path.Join(tmpDirectory, fmt.Sprintf("/vt_%010d/querylog.txt", tabletUID)),
		Directory:                   path.Join(os.Getenv("VTDATAROOT"), fmt.Sprintf("/vt_%010d", tabletUID)),
		TabletPath:                  fmt.Sprintf("%s-%010d", cell, tabletUID),
		ServiceMap:                  "grpc-queryservice,grpc-tabletmanager,grpc-updatestream",
		LogDir:                      tmpDirectory,
		Shard:                       shard,
		TabletHostname:              hostname,
		Keyspace:                    keyspace,
		TabletType:                  "replica",
		CommonArg:                   *vtctl,
		HealthCheckInterval:         5,
		BackupStorageImplementation: "file",
		FileBackupStorageRoot:       path.Join(os.Getenv("VTDATAROOT"), "/backups"),
		Port:                        port,
		GrpcPort:                    grpcPort,
		PidFile:                     path.Join(os.Getenv("VTDATAROOT"), fmt.Sprintf("/vt_%010d/vttablet.pid", tabletUID)),
		VtctldAddress:               fmt.Sprintf("http://%s:%d", hostname, vtctldPort),
		ExtraArgs:                   extraArgs,
	}

	if tabletType == "rdonly" {
		vttablet.TabletType = tabletType
	}
	vttablet.VerifyURL = fmt.Sprintf("http://%s:%d/debug/vars", hostname, port)

	return vttablet
}
