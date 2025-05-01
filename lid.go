package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

// LidConfig represents the root configuration structure
type LidConfig struct {
	Settings LidSettings           `yaml:"settings"`
	Services map[string]ServiceDef `yaml:"services"`
}

// LidSettings contains global settings
type LidSettings struct {
	DeploymentsDir string `yaml:"deployments_dir"`
	StateFile      string `yaml:"state_file"`
}

// ServiceDef defines a service configuration
type ServiceDef struct {
	Command   string            `yaml:"command"`
	DependsOn []string          `yaml:"depends_on"`
	Env       map[string]string `yaml:"env"`
	EnvFile   string            `yaml:"env_file"`
	PreStart  string            `yaml:"pre_start"`
	Restart   string            `yaml:"restart"`
	User      string            `yaml:"user"`
	Group     string            `yaml:"group"`
}

// DeploymentState tracks the current and previous deployments
type DeploymentState struct {
	CurrentCommit  string    `json:"current_commit"`
	PreviousCommit string    `json:"previous_commit"`
	Timestamp      time.Time `json:"timestamp"`
	Services       []string  `json:"services"`
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	switch cmd {
	case "check":
		checkCommand()
	case "status":
		statusCommand()
	case "deploy":
		if len(os.Args) < 3 {
			fmt.Println("Error: Missing commit hash")
			fmt.Println("Usage: lid deploy <commit-hash>")
			os.Exit(1)
		}
		deployCommand(os.Args[2])
	case "rollback":
		rollbackCommand()
	default:
		fmt.Printf("Unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

// printUsage prints the CLI usage instructions
func printUsage() {
	fmt.Println("Lid - Simple service management for monorepos")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  lid check                    Check configuration and service status")
	fmt.Println("  lid status                   Show status of all services")
	fmt.Println("  lid deploy <commit-hash>     Deploy services from specified commit")
	fmt.Println("  lid rollback                 Revert to previous deployment")
}

// checkCommand validates the configuration and checks for systemd service discrepancies
func checkCommand() {
	config, err := loadConfig()
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("Configuration check:")
	fmt.Printf("  Deployments directory: %s\n", config.Settings.DeploymentsDir)

	// Check if deployments directory exists
	if _, err := os.Stat(config.Settings.DeploymentsDir); os.IsNotExist(err) {
		fmt.Printf("  Warning: Deployments directory does not exist: %s\n", config.Settings.DeploymentsDir)
	} else {
		fmt.Printf("  Deployments directory exists: %s\n", config.Settings.DeploymentsDir)
	}

	// Check services
	fmt.Println("\nServices:")
	for name, service := range config.Services {
		fmt.Printf("  %s:\n", name)
		fmt.Printf("    Command: %s\n", service.Command)

		// Check systemd service
		exists, running := checkSystemdService(name)
		if exists {
			fmt.Printf("    Systemd service: Exists")
			if running {
				fmt.Println(" (Active)")
			} else {
				fmt.Println(" (Inactive)")
			}
		} else {
			fmt.Println("    Systemd service: Not installed")
		}

		// Check dependencies
		if len(service.DependsOn) > 0 {
			fmt.Printf("    Depends on: %s\n", strings.Join(service.DependsOn, ", "))

			// Verify dependencies exist in config
			for _, dep := range service.DependsOn {
				if _, exists := config.Services[dep]; !exists {
					fmt.Printf("    Warning: Dependency '%s' is not defined in Lidfile\n", dep)
				}
			}
		}
	}

	// Check for dependency cycles
	_, err = getSortedServices(config)
	if err != nil {
		fmt.Printf("\nWarning: %v\n", err)
	} else {
		fmt.Println("\nNo dependency cycles detected.")
	}
}

// statusCommand displays the current status of all services
func statusCommand() {
	config, err := loadConfig()
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Get current deployment if available
	state, _ := loadState(config.Settings.StateFile)
	var currentDeployment string
	if state != nil {
		currentDeployment = state.CurrentCommit
	}

	fmt.Println("Service Status:")
	if currentDeployment != "" {
		fmt.Printf("Current deployment: %s\n", currentDeployment)
	}
	fmt.Println("-------------------------------------------------------------------------------")
	fmt.Printf("%-20s %-10s %-15s %-15s\n", "SERVICE", "STATUS", "SINCE", "MEMORY")
	fmt.Println("-------------------------------------------------------------------------------")

	args := make([]string, len(config.Services)+1)
	args = append(args, "status")
	for name := range config.Services {
		args = append(args, name)
		status, since, memory := getServiceStatus(name)
		fmt.Printf("%-20s %-10s %-15s %-15s\n", name, status, since, memory)
	}

	exec.Command("systemctl", args...)
}

// deployCommand deploys services from the specified commit
func deployCommand(commitHash string) {
	config, err := loadConfig()
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Validate that the commit hash exists in deployments directory
	deployDir := filepath.Join(config.Settings.DeploymentsDir, commitHash)
	if _, err := os.Stat(deployDir); os.IsNotExist(err) {
		fmt.Printf("Error: Deployment for commit %s not found in %s\n", commitHash, config.Settings.DeploymentsDir)
		os.Exit(1)
	}

	// Get current state if available
	state, _ := loadState(config.Settings.StateFile)

	// Create new state
	newState := DeploymentState{
		CurrentCommit:  commitHash,
		PreviousCommit: "",
		Timestamp:      time.Now(),
		Services:       make([]string, 0),
	}

	// Set previous commit if we had state
	if state != nil {
		newState.PreviousCommit = state.CurrentCommit
	}

	// Get services in dependency order
	orderedServices, err := getSortedServices(config)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Deploying services from commit %s...\n", commitHash)

	// Deploy each service
	for _, serviceName := range orderedServices {
		service, ok := config.Services[serviceName]
		if !ok {
			continue // Skip if service was removed from config
		}

		fmt.Printf("Deploying %s...\n", serviceName)

		// Create systemd service file
		serviceFile := generateServiceFile(serviceName, service, commitHash, config.Settings.DeploymentsDir)

		// Write to temp file first
		tempFile := fmt.Sprintf("/tmp/%s.service", serviceName)
		if err := os.WriteFile(tempFile, []byte(serviceFile), 0644); err != nil {
			fmt.Printf("Error writing service file: %v\n", err)
			os.Exit(1)
		}

		// Move to systemd directory (requires root)
		systemdPath := fmt.Sprintf("/etc/systemd/system/%s.service", serviceName)
		if err := exec.Command("sudo", "mv", tempFile, systemdPath).Run(); err != nil {
			fmt.Printf("Error installing service file (run with sudo?): %v\n", err)
			os.Exit(1)
		}

		// Stop current service if running
		exec.Command("sudo", "systemctl", "stop", serviceName).Run()

		// Reload daemon
		if err := exec.Command("sudo", "systemctl", "daemon-reload").Run(); err != nil {
			fmt.Printf("Error reloading systemd: %v\n", err)
			os.Exit(1)
		}

		// Start service
		cmd := exec.Command("sudo", "systemctl", "start", serviceName)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Printf("Error starting service: %v\n", err)
			os.Exit(1)
		}

		// Enable service
		if err := exec.Command("sudo", "systemctl", "enable", serviceName).Run(); err != nil {
			fmt.Printf("Warning: Could not enable service to start on boot: %v\n", err)
		}

		newState.Services = append(newState.Services, serviceName)
		fmt.Printf("Service %s deployed successfully\n", serviceName)
	}

	// Save new state
	if err := saveState(config.Settings.StateFile, &newState); err != nil {
		fmt.Printf("Warning: Could not save state file: %v\n", err)
	}

	fmt.Printf("\nDeployment of commit %s completed successfully\n", commitHash)
}

// rollbackCommand reverts to the previous deployment
func rollbackCommand() {
	config, err := loadConfig()
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Load state
	state, err := loadState(config.Settings.StateFile)
	if err != nil {
		fmt.Printf("Error: Could not load state file: %v\n", err)
		os.Exit(1)
	}

	if state.PreviousCommit == "" {
		fmt.Println("Error: No previous deployment found to roll back to")
		os.Exit(1)
	}

	fmt.Printf("Rolling back from %s to %s\n", state.CurrentCommit, state.PreviousCommit)
	deployCommand(state.PreviousCommit)
}

// loadConfig loads and parses the Lidfile
func loadConfig() (*LidConfig, error) {
	// Try to find Lidfile.yaml
	var configFile string
	for _, name := range []string{"Lidfile.yaml", "Lidfile.yml", "lidfile.yaml", "lidfile.yml"} {
		if _, err := os.Stat(name); err == nil {
			configFile = name
			break
		}
	}

	if configFile == "" {
		return nil, fmt.Errorf("Lidfile not found. Create a Lidfile.yaml in your project root")
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("Could not read %s: %v", configFile, err)
	}

	var config LidConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("Could not parse %s: %v", configFile, err)
	}

	// Set defaults
	if config.Settings.DeploymentsDir == "" {
		config.Settings.DeploymentsDir = "./deployments"
	}
	if config.Settings.StateFile == "" {
		config.Settings.StateFile = ".lid-state.json"
	}

	return &config, nil
}

// loadState loads the deployment state from the state file
func loadState(stateFile string) (*DeploymentState, error) {
	if stateFile == "" {
		stateFile = ".lid-state.json"
	}

	// Check if file exists
	if _, err := os.Stat(stateFile); os.IsNotExist(err) {
		return nil, fmt.Errorf("State file does not exist")
	}

	// Read file
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return nil, fmt.Errorf("Could not read state file: %v", err)
	}

	// Parse JSON
	var state DeploymentState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("Could not parse state file: %v", err)
	}

	return &state, nil
}

// saveState saves the deployment state to the state file
func saveState(stateFile string, state *DeploymentState) error {
	if stateFile == "" {
		stateFile = ".lid-state.json"
	}

	// Marshal to JSON
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("Could not marshal state: %v", err)
	}

	// Write file
	if err := os.WriteFile(stateFile, data, 0644); err != nil {
		return fmt.Errorf("Could not write state file: %v", err)
	}

	return nil
}

// checkSystemdService checks if a systemd service exists and is running
func checkSystemdService(name string) (exists bool, running bool) {
	// Check if service exists
	cmd := exec.Command("systemctl", "list-unit-files", fmt.Sprintf("%s.service", name))
	output, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(output), fmt.Sprintf("%s.service", name)) {
		return false, false
	}

	// Check if service is running
	cmd = exec.Command("systemctl", "is-active", fmt.Sprintf("%s.service", name))
	output, err = cmd.CombinedOutput()
	if err == nil && strings.TrimSpace(string(output)) == "active" {
		return true, true
	}

	return true, false
}

// getServiceStatus returns the status, uptime and memory usage of a service
func getServiceStatus(name string) (status string, since string, memory string) {
	// Default values
	status = "inactive"
	since = "-"
	memory = "-"

	// Get status
	cmd := exec.Command("systemctl", "is-active", fmt.Sprintf("%s.service", name))
	output, err := cmd.CombinedOutput()
	if err == nil {
		status = strings.TrimSpace(string(output))
	}

	// If service is not active, return early
	if status != "active" {
		return status, since, memory
	}

	// Get since time if active
	cmd = exec.Command("systemctl", "show", "-p", "ActiveEnterTimestamp", fmt.Sprintf("%s.service", name))
	output, err = cmd.CombinedOutput()
	if err == nil {
		parts := strings.Split(strings.TrimSpace(string(output)), "=")
		if len(parts) > 1 {
			// Format timestamp
			timestamp := parts[1]
			cmd = exec.Command("date", "-d", timestamp, "+%Y-%m-%d %H:%M:%S")
			output, err := cmd.CombinedOutput()
			if err == nil {
				since = strings.TrimSpace(string(output))
			} else {
				since = timestamp
			}
		}
	}

	// Get memory usage
	cmd = exec.Command("systemctl", "show", "-p", "MemoryCurrent", fmt.Sprintf("%s.service", name))
	output, err = cmd.CombinedOutput()
	if err == nil {
		parts := strings.Split(strings.TrimSpace(string(output)), "=")
		if len(parts) > 1 && parts[1] != "[not set]" {
			// Convert to MB
			memBytes := parts[1]
			if memBytes != "18446744073709551615" { // infinity
				cmd = exec.Command("numfmt", "--to=iec", "--suffix=B", memBytes)
				output, err := cmd.CombinedOutput()
				if err == nil {
					memory = strings.TrimSpace(string(output))
				} else {
					memory = memBytes + " B"
				}
			}
		}
	}

	return status, since, memory
}

// getSortedServices returns a list of services sorted by dependency order
func getSortedServices(config *LidConfig) ([]string, error) {
	// Build dependency graph
	graph := make(map[string][]string)
	for name, service := range config.Services {
		deps := make([]string, 0)
		if service.DependsOn != nil {
			deps = service.DependsOn
		}
		graph[name] = deps
	}

	// Detect cycles
	visited := make(map[string]bool)
	temp := make(map[string]bool)
	var cycle []string

	var checkCycle func(string, *[]string) bool
	checkCycle = func(node string, path *[]string) bool {
		if temp[node] {
			*path = append(*path, node)
			return true
		}
		if visited[node] {
			return false
		}
		*path = append(*path, node)
		temp[node] = true

		for _, neighbor := range graph[node] {
			if checkCycle(neighbor, path) {
				return true
			}
		}

		*path = (*path)[:len(*path)-1]
		temp[node] = false
		visited[node] = true
		return false
	}

	// Check for cycles
	for node := range graph {
		if !visited[node] {
			path := make([]string, 0)
			if checkCycle(node, &path) {
				for i := len(path) - 1; i >= 0; i-- {
					if path[i] == path[len(path)-1] {
						cycle = path[i:]
						break
					}
				}
				return nil, fmt.Errorf("Dependency cycle detected: %s", strings.Join(cycle, " -> "))
			}
		}
	}

	// Topological sort
	var result []string
	visited = make(map[string]bool)

	var visit func(string)
	visit = func(node string) {
		if visited[node] {
			return
		}
		visited[node] = true

		for _, neighbor := range graph[node] {
			visit(neighbor)
		}
		result = append(result, node)
	}

	// Visit all nodes
	for node := range graph {
		visit(node)
	}

	// Reverse to get correct order (dependencies first)
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	return result, nil
}

// generateServiceFile creates a systemd unit file for a service
func generateServiceFile(name string, service ServiceDef, commitHash string, deploymentsDir string) string {
	var sb strings.Builder

	// [Unit] section
	sb.WriteString("[Unit]\n")
	sb.WriteString(fmt.Sprintf("Description=%s\n", name))
	sb.WriteString("After=network.target")

	// Add dependencies
	if service.DependsOn != nil && len(service.DependsOn) > 0 {
		for _, dep := range service.DependsOn {
			sb.WriteString(fmt.Sprintf(" %s.service", dep))
		}
		sb.WriteString("\n")

		sb.WriteString("Requires=")
		for i, dep := range service.DependsOn {
			if i > 0 {
				sb.WriteString(" ")
			}
			sb.WriteString(fmt.Sprintf("%s.service", dep))
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	// [Service] section
	sb.WriteString("[Service]\n")

	// Working directory

	workingDir := filepath.Join(deploymentsDir, commitHash, name)
	if !filepath.IsAbs(workingDir) {
		// Get current working directory as base
		cwd, err := os.Getwd()
		if err != nil {
			// Fallback if we can't get current directory
			cwd = "/"
		}
		workingDir = filepath.Join(cwd, workingDir)
	}
	sb.WriteString(fmt.Sprintf("WorkingDirectory=%s\n", workingDir))

	// Pre-start script
	if service.PreStart != "" {
		preStartPath := filepath.Join(workingDir, service.PreStart)
		sb.WriteString(fmt.Sprintf("ExecStartPre=%s\n", preStartPath))
	}

	// Main command
	sb.WriteString(fmt.Sprintf("ExecStart=%s\n", filepath.Join(workingDir, service.Command)))

	// Environment variables
	if service.Env != nil && len(service.Env) > 0 {
		for key, value := range service.Env {
			sb.WriteString(fmt.Sprintf("Environment=\"%s=%s\"\n", key, value))
		}
	}

	// Environment file
	if service.EnvFile != "" {
		envFilePath := service.EnvFile
		if !filepath.IsAbs(envFilePath) {
			// If relative path, make it relative to the service directory
			envFilePath = filepath.Join(deploymentsDir, commitHash, name, envFilePath)
		}
		sb.WriteString(fmt.Sprintf("EnvironmentFile=%s\n", envFilePath))
	}

	// Restart policy
	if service.Restart != "" {
		sb.WriteString(fmt.Sprintf("Restart=%s\n", service.Restart))
	} else {
		sb.WriteString("Restart=on-failure\n")
	}

	// User and group
	if service.User != "" {
		sb.WriteString(fmt.Sprintf("User=%s\n", service.User))
	}
	if service.Group != "" {
		sb.WriteString(fmt.Sprintf("Group=%s\n", service.Group))
	}

	// Logging
	sb.WriteString("StandardOutput=journal\n")
	sb.WriteString("StandardError=journal\n")
	sb.WriteString("\n")

	// [Install] section
	sb.WriteString("[Install]\n")
	sb.WriteString("WantedBy=multi-user.target\n")

	return sb.String()
}
