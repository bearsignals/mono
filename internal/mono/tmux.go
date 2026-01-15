package mono

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const tmuxTimeout = 5 * time.Second

func SessionName(envName string) string {
	return fmt.Sprintf("mono-%s", envName)
}

func SessionExists(sessionName string) bool {
	err := Command("tmux", "has-session", "-t", sessionName).
		Timeout(tmuxTimeout).
		Run()
	return err == nil
}

func CreateSession(sessionName, workDir string, envVars []string) error {
	args := []string{"new-session", "-d", "-s", sessionName, "-c", workDir}
	for _, envVar := range envVars {
		args = append(args, "-e", envVar)
	}

	output, err := Command("tmux", args...).
		Timeout(tmuxTimeout).
		CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to create session: %s: %w", string(output), err)
	}

	return nil
}

func SendKeys(sessionName, keys string) error {
	Command("tmux", "send-keys", "-t", sessionName, "C-u").
		Timeout(tmuxTimeout).
		Run()
	return Command("tmux", "send-keys", "-t", sessionName, keys, "Enter").
		Timeout(tmuxTimeout).
		Run()
}

func KillSession(sessionName string) error {
	if !SessionExists(sessionName) {
		return nil
	}
	return Command("tmux", "kill-session", "-t", sessionName).
		Timeout(tmuxTimeout).
		Run()
}

func IsInsideTmux() bool {
	return os.Getenv("TMUX") != ""
}

func ListMonoSessions() ([]string, error) {
	output, err := Command("tmux", "list-sessions", "-F", "#{session_name}").
		Timeout(tmuxTimeout).
		Output()
	if err != nil {
		return nil, nil
	}

	var sessions []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.HasPrefix(line, "mono-") {
			sessions = append(sessions, line)
		}
	}
	return sessions, nil
}

type TmuxManager struct {
	sessionName string
	workDir     string
	config      TmuxConfig
}

func NewTmuxManager(sessionName, workDir string, config TmuxConfig) *TmuxManager {
	return &TmuxManager{
		sessionName: sessionName,
		workDir:     workDir,
		config:      config,
	}
}

func (tm *TmuxManager) CreateSession(envVars []string) error {
	return CreateSession(tm.sessionName, tm.workDir, envVars)
}

func (tm *TmuxManager) SessionExists() bool {
	return SessionExists(tm.sessionName)
}

func (tm *TmuxManager) KillSession() error {
	return KillSession(tm.sessionName)
}

func (tm *TmuxManager) Run(scriptPath string) error {
	if tm.config.Run.OnConflict == "respawn" {
		return tm.respawn(fmt.Sprintf("source %s", scriptPath))
	}
	tm.interrupt()
	tm.sendKeys(fmt.Sprintf("cd %q", tm.workDir))
	return tm.sendKeys("source " + scriptPath)
}

func (tm *TmuxManager) interrupt() error {
	return Command("tmux", "send-keys", "-t", tm.sessionName, "C-c").
		Timeout(tmuxTimeout).
		Run()
}

func (tm *TmuxManager) respawn(cmd string) error {
	fullCmd := fmt.Sprintf("cd %q && %s", tm.workDir, cmd)
	return Command("tmux", "respawn-pane", "-k", "-t", tm.sessionName, fullCmd).
		Timeout(tmuxTimeout).
		Run()
}

func (tm *TmuxManager) sendKeys(keys string) error {
	return SendKeys(tm.sessionName, keys)
}
