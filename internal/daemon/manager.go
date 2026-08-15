package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const ServiceName = "pairroom"

var proxyEnvironmentNames = []string{
	"http_proxy", "https_proxy", "no_proxy",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"all_proxy", "ALL_PROXY",
}

type Config struct {
	BinaryPath  string
	WorkDir     string
	LogFile     string
	LogMaxSize  int64
	LogBackups  int
	ControlFile string
	StopTimeout time.Duration
	Args        []string
	EnvPATH     string
	EnvExtra    map[string]string
}

type Status struct {
	Installed bool
	Running   bool
	PID       int
	Platform  string
}

type Manager interface {
	Install(Config) error
	Uninstall() error
	Start() error
	Stop() error
	Restart() error
	Status() (*Status, error)
	Platform() string
}

type Meta struct {
	LogFile            string `json:"log_file"`
	LogMaxSize         int64  `json:"log_max_size"`
	LogBackups         int    `json:"log_backups"`
	ControlFile        string `json:"control_file"`
	DataRoot           string `json:"data_root,omitempty"`
	StopTimeoutSeconds int64  `json:"stop_timeout_seconds"`
	WorkDir            string `json:"work_dir"`
	BinaryPath         string `json:"binary_path"`
	Platform           string `json:"platform"`
	InstalledAt        string `json:"installed_at"`
}

func NewManager() (Manager, error) {
	return newPlatformManager()
}

func DefaultDataDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locate user config directory: %w", err)
	}
	return filepath.Join(base, "pairroom"), nil
}

func DefaultLogFile() (string, error) {
	root, err := DefaultDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "logs", "service.log"), nil
}

func DefaultControlFile() (string, error) {
	root, err := DefaultDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "daemon.stop"), nil
}

func MetaPath() (string, error) {
	root, err := DefaultDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "daemon.json"), nil
}

func Resolve(cfg *Config) error {
	if cfg == nil {
		return errors.New("daemon configuration is required")
	}
	if strings.TrimSpace(cfg.BinaryPath) == "" {
		executable, err := os.Executable()
		if err != nil {
			return fmt.Errorf("locate pairroom executable: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(executable); err == nil {
			executable = resolved
		}
		cfg.BinaryPath = executable
	}
	var err error
	cfg.BinaryPath, err = absoluteFile(cfg.BinaryPath, "pairroom executable")
	if err != nil {
		return err
	}

	if strings.TrimSpace(cfg.WorkDir) == "" {
		cfg.WorkDir, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("locate daemon working directory: %w", err)
		}
	}
	cfg.WorkDir, err = absoluteDirectory(cfg.WorkDir, "daemon working directory")
	if err != nil {
		return err
	}

	if strings.TrimSpace(cfg.LogFile) == "" {
		cfg.LogFile, err = DefaultLogFile()
		if err != nil {
			return err
		}
	}
	cfg.LogFile, err = absolutePathFrom(cfg.WorkDir, cfg.LogFile)
	if err != nil {
		return fmt.Errorf("resolve daemon log file: %w", err)
	}
	if strings.TrimSpace(cfg.ControlFile) == "" {
		cfg.ControlFile, err = DefaultControlFile()
		if err != nil {
			return err
		}
	}
	cfg.ControlFile, err = absolutePathFrom(cfg.WorkDir, cfg.ControlFile)
	if err != nil {
		return fmt.Errorf("resolve daemon control file: %w", err)
	}
	for _, value := range append([]string{cfg.BinaryPath, cfg.WorkDir, cfg.LogFile, cfg.ControlFile}, cfg.Args...) {
		if strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("daemon paths and arguments must not contain control characters")
		}
	}
	if len(cfg.Args) == 0 {
		return errors.New("daemon command arguments are required")
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 11 * time.Minute
	}
	if cfg.LogMaxSize <= 0 {
		cfg.LogMaxSize = DefaultLogMaxSize
	}
	if cfg.LogBackups <= 0 {
		cfg.LogBackups = DefaultLogMaxBackups
	}
	if cfg.EnvPATH == "" {
		cfg.EnvPATH = os.Getenv("PATH")
	}
	if cfg.EnvExtra == nil {
		cfg.EnvExtra = make(map[string]string)
		for _, name := range proxyEnvironmentNames {
			if value := os.Getenv(name); value != "" {
				cfg.EnvExtra[name] = value
			}
		}
	}
	return nil
}

func SaveMeta(meta *Meta) error {
	if meta == nil {
		return errors.New("daemon metadata is required")
	}
	path, err := MetaPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create daemon metadata directory: %w", err)
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encode daemon metadata: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write daemon metadata: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("protect daemon metadata: %w", err)
	}
	return nil
}

func LoadMeta() (*Meta, error) {
	path, err := MetaPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("decode daemon metadata: %w", err)
	}
	return &meta, nil
}

func RemoveMeta() error {
	path, err := MetaPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove daemon metadata: %w", err)
	}
	return nil
}

func RemoveControlFile() error {
	path, err := DefaultControlFile()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove daemon control file: %w", err)
	}
	return nil
}

func RequestStop() error {
	path, err := DefaultControlFile()
	if err != nil {
		return err
	}
	if meta, metaErr := LoadMeta(); metaErr == nil && strings.TrimSpace(meta.ControlFile) != "" {
		path = meta.ControlFile
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create daemon control directory: %w", err)
	}
	content := []byte(time.Now().UTC().Format(time.RFC3339Nano) + "\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("request daemon stop: %w", err)
	}
	return nil
}

func SortedEnvironment(cfg Config) [][2]string {
	values := make(map[string]string, len(cfg.EnvExtra)+4)
	if cfg.EnvPATH != "" {
		values["PATH"] = cfg.EnvPATH
	}
	if cfg.LogFile != "" {
		values[LogFileEnvironment] = cfg.LogFile
		values[LogSizeEnvironment] = strconv.FormatInt(cfg.LogMaxSize, 10)
		values[LogBackupEnvironment] = strconv.Itoa(cfg.LogBackups)
	}
	for key, value := range cfg.EnvExtra {
		if value == "" || !ValidEnvironmentName(key) || key == "PATH" || key == LogFileEnvironment || key == LogSizeEnvironment || key == LogBackupEnvironment {
			continue
		}
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([][2]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, [2]string{key, values[key]})
	}
	return result
}

func ValidEnvironmentName(value string) bool {
	if value == "" {
		return false
	}
	for index, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (index > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func absoluteFile(input, label string) (string, error) {
	absolute, err := filepath.Abs(input)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", label, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory: %s", label, absolute)
	}
	return absolute, nil
}

func absoluteDirectory(input, label string) (string, error) {
	absolute, err := filepath.Abs(input)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", label, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory: %s", label, absolute)
	}
	return absolute, nil
}

func absolutePathFrom(base, input string) (string, error) {
	if !filepath.IsAbs(input) {
		input = filepath.Join(base, input)
	}
	absolute, err := filepath.Abs(input)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func durationSeconds(value time.Duration) int64 {
	seconds := int64(value / time.Second)
	if value%time.Second != 0 {
		seconds++
	}
	return seconds
}
