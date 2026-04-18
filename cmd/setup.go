package cmd

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const serviceTemplate = `[Unit]
Description=Lattice Runner
After=network.target docker.service
Requires=docker.service

[Service]
Type=simple
WorkingDirectory=/opt/lattice-runner
EnvironmentFile=/opt/lattice-runner/.env
ExecStart=/opt/lattice-runner/lattice-runner
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`

// RunSetup walks the user through configuring and installing the runner.
func RunSetup() {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════╗")
	fmt.Println("║        Lattice Runner Setup              ║")
	fmt.Println("╚══════════════════════════════════════════╝")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	// Orchestrator URL
	orchestratorURL := prompt(reader, "Orchestrator URL", "wss://lattice-api.appleby.cloud/ws/worker")

	// Worker token
	workerToken := promptRequired(reader, "Worker Token (from dashboard)")

	// Worker name
	defaultName, _ := os.Hostname()
	workerName := prompt(reader, "Worker Name", defaultName)

	fmt.Println()

	// Write env file
	envContent := fmt.Sprintf("ORCHESTRATOR_URL=%s\nWORKER_TOKEN=%s\nWORKER_NAME=%s\n",
		orchestratorURL, workerToken, workerName)

	// Determine install location
	installDir := "/opt/lattice-runner"
	if runtime.GOOS != "linux" {
		// macOS or other - just write .env locally
		writeLocalEnv(envContent)
		return
	}

	installService := promptYesNo(reader, "Install as systemd service?", true)
	if !installService {
		writeLocalEnv(envContent)
		return
	}

	fmt.Println()
	fmt.Printf("Installing to %s...\n", installDir)

	// Create install directory
	if err := os.MkdirAll(installDir, 0755); err != nil {
		fmt.Printf("Failed to create %s: %v\n", installDir, err)
		fmt.Println("Try running with sudo: sudo lattice-runner setup")
		return
	}

	// Write env file
	envPath := filepath.Join(installDir, ".env")
	if err := os.WriteFile(envPath, []byte(envContent), 0600); err != nil {
		fmt.Printf("Failed to write %s: %v\n", envPath, err)
		return
	}
	fmt.Printf("  Wrote %s\n", envPath)

	// Copy binary
	execPath, err := os.Executable()
	if err != nil {
		fmt.Printf("Failed to find current binary: %v\n", err)
		return
	}
	destBinary := filepath.Join(installDir, "lattice-runner")
	if execPath != destBinary {
		input, err := os.ReadFile(execPath)
		if err != nil {
			fmt.Printf("Failed to read binary: %v\n", err)
			return
		}
		if err := os.WriteFile(destBinary, input, 0755); err != nil {
			fmt.Printf("Failed to copy binary: %v\n", err)
			return
		}
		fmt.Printf("  Copied binary to %s\n", destBinary)
	}

	// Write systemd service
	servicePath := "/etc/systemd/system/lattice-runner.service"
	if err := os.WriteFile(servicePath, []byte(serviceTemplate), 0644); err != nil {
		fmt.Printf("Failed to write %s: %v\n", servicePath, err)
		return
	}
	fmt.Printf("  Wrote %s\n", servicePath)

	// Reload and enable
	run("systemctl", "daemon-reload")
	run("systemctl", "enable", "lattice-runner")
	run("systemctl", "start", "lattice-runner")

	fmt.Println()
	fmt.Println("Lattice Runner installed and started.")
	fmt.Println()
	fmt.Println("Useful commands:")
	fmt.Println("  sudo systemctl status lattice-runner    # check status")
	fmt.Println("  sudo journalctl -u lattice-runner -f    # view logs")
	fmt.Println("  sudo systemctl restart lattice-runner   # restart")
	fmt.Println("  sudo systemctl stop lattice-runner      # stop")
	fmt.Println()
}

func prompt(reader *bufio.Reader, label, defaultVal string) string {
	if defaultVal != "" {
		fmt.Printf("  %s [%s]: ", label, defaultVal)
	} else {
		fmt.Printf("  %s: ", label)
	}
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(input)
	if input == "" {
		return defaultVal
	}
	return input
}

func promptRequired(reader *bufio.Reader, label string) string {
	for {
		fmt.Printf("  %s: ", label)
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		if input != "" {
			return input
		}
		fmt.Println("  This field is required.")
	}
}

func promptYesNo(reader *bufio.Reader, label string, defaultYes bool) bool {
	hint := "Y/n"
	if !defaultYes {
		hint = "y/N"
	}
	fmt.Printf("  %s [%s]: ", label, hint)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSpace(strings.ToLower(input))
	if input == "" {
		return defaultYes
	}
	return input == "y" || input == "yes"
}

func writeLocalEnv(content string) {
	if err := os.WriteFile(".env", []byte(content), 0600); err != nil {
		fmt.Printf("Failed to write .env: %v\n", err)
		return
	}
	fmt.Println("Wrote .env file. Start the runner with:")
	fmt.Println()
	fmt.Println("  source .env && go run .")
	fmt.Println()
}

func run(name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}
