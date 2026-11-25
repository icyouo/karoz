package device

import "time"

type BackendKind string

const (
	BackendADBHost BackendKind = "adb_host"
	BackendAVD     BackendKind = "avd"
	BackendRedroid BackendKind = "redroid"
)

type DeviceStatus string

const (
	StatusOffline  DeviceStatus = "offline"
	StatusStarting DeviceStatus = "starting"
	StatusReady    DeviceStatus = "ready"
	StatusRunning  DeviceStatus = "running"
	StatusStopped  DeviceStatus = "stopped"
	StatusError    DeviceStatus = "error"
)

type Device struct {
	ID             string       `json:"id"`
	ProjectID      string       `json:"project_id,omitempty"`
	Name           string       `json:"name"`
	Serial         string       `json:"serial,omitempty"`
	AVDName        string       `json:"avd_name,omitempty"`
	ContainerID    string       `json:"container_id,omitempty"`
	Backend        BackendKind  `json:"backend"`
	Status         DeviceStatus `json:"status"`
	Width          int          `json:"width"`
	Height         int          `json:"height"`
	Model          string       `json:"model,omitempty"`
	AndroidVersion string       `json:"android_version,omitempty"`
	Error          string       `json:"error,omitempty"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

type StartRequest struct {
	Backend  BackendKind `json:"backend,omitempty"`
	DeviceID string      `json:"device_id,omitempty"`
	Serial   string      `json:"serial,omitempty"`
	AVDName  string      `json:"avd_name,omitempty"`
	Name     string      `json:"name,omitempty"`
	Image    string      `json:"image,omitempty"`
	Headless bool        `json:"headless,omitempty"`
}

type ActionRequest struct {
	Kind       string `json:"kind"`
	X          *int   `json:"x,omitempty"`
	Y          *int   `json:"y,omitempty"`
	FromX      *int   `json:"from_x,omitempty"`
	FromY      *int   `json:"from_y,omitempty"`
	ToX        *int   `json:"to_x,omitempty"`
	ToY        *int   `json:"to_y,omitempty"`
	DurationMS int    `json:"duration_ms,omitempty"`
	Text       string `json:"text,omitempty"`
	Key        string `json:"key,omitempty"`
	Package    string `json:"package,omitempty"`
	Activity   string `json:"activity,omitempty"`
	URL        string `json:"url,omitempty"`
	MS         int    `json:"ms,omitempty"`
}

type TouchEvent struct {
	Phase          string `json:"phase"`
	X              int    `json:"x"`
	Y              int    `json:"y"`
	ViewportWidth  int    `json:"viewport_width,omitempty"`
	ViewportHeight int    `json:"viewport_height,omitempty"`
}
