package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var devComposeArgs = []string{"-f", "docker-compose.yml", "-f", "docker-compose.override.yml"}

func runDev(p paths) error {
	if err := loadEnv(filepath.Join(p.infra, ".env")); err != nil {
		return err
	}

	// MongoDB 5+ requires AVX; fall back to 4.4 on older CPUs.
	img := "mongo:4.4"
	if hasAVX() {
		img = "mongo:7"
	} else {
		fmt.Println("[nucleus] No AVX detected — using mongo:4.4")
	}
	os.Setenv("MONGO_IMAGE", img)
	if err := persistEnvVar(filepath.Join(p.infra, ".env"), "MONGO_IMAGE", img); err != nil {
		return err
	}

	if err := runGenerate(p); err != nil {
		return err
	}

	running, err := devRunning(p)
	if err != nil {
		return err
	}
	if running {
		fmt.Printf("\n%s%s✓ Dev stack already running.%s Use 'infra/nucleus down' to stop it.\n", cBold, cGreen, cReset)
		return nil
	}

	step("Starting dev stack")
	up := exec.Command("docker", append(append([]string{"compose"}, devComposeArgs...), "up", "-d")...)
	up.Dir = p.infra
	up.Stdout, up.Stderr, up.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := up.Run(); err != nil {
		return fmt.Errorf("docker compose up failed: %w", err)
	}

	fmt.Printf("\n%s%s✓ Dev stack is up.%s\n", cBold, cGreen, cReset)
	fmt.Println("  Hub → http://localhost/")
	return nil
}

func devRunning(p paths) (bool, error) {
	args := append(append([]string{"compose"}, devComposeArgs...), "ps", "--status", "running", "--quiet")
	cmd := exec.Command("docker", args...)
	cmd.Dir = p.infra
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("docker compose ps failed: %w", err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func persistEnvVar(path, key, val string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return os.WriteFile(path, []byte(key+"="+val+"\n"), 0o644)
		}
		return err
	}
	if strings.TrimSpace(string(data)) == "" {
		return os.WriteFile(path, []byte(key+"="+val+"\n"), 0o644)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	found := false
	for i := range lines {
		if strings.HasPrefix(lines[i], key+"=") {
			lines[i] = key + "=" + val
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, key+"="+val)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}
