package main

import (
	"bytes"
	"fmt"
	"github.com/turbot/steampipe-plugin-aws/aws"
	"github.com/turbot/steampipe-plugin-sdk/v5/plugin"
	"os/exec"
	"strings"
)

func main() {
	if running, err := checkIfPluginRunning("aws.plugin"); err != nil {
		fmt.Println("Error checking if plugin is running:", err)
	} else if running {
		fmt.Println("Plugin is already running")
		return
	}

	plugin.Serve(&plugin.ServeOpts{
		PluginFunc: aws.Plugin})
}

func checkIfPluginRunning(pluginName string) (bool, error) {
	// Execute the ps -ef command to list all processes
	cmd := exec.Command("ps", "-ef")
	var out bytes.Buffer
	cmd.Stdout = &out

	// Run the command and check for errors
	err := cmd.Run()
	if err != nil {
		return false, fmt.Errorf("error running ps command: %v", err)
	}

	// Parse the output
	lines := strings.Split(out.String(), "\n")
	for _, line := range lines {
		// Check if the line contains the plugin name (e.g., aws.plugin)
		if strings.Contains(line, pluginName) {
			return true, nil
		}
	}

	// Return false if the plugin process was not found
	return false, nil
}
