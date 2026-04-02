package handlers

import (
	"net/http"

	"github.com/ttalvac/bump-server/db"
)

type ConfigHandler struct {
	queries       *db.Queries
	timeWindowSec int
	minRSSI       int
	minAppVersion int
	maxSessions   int
	killSwitch    bool
}

func NewConfigHandler(queries *db.Queries, timeWindowSec, minRSSI, minAppVersion, maxSessions int, killSwitch bool) *ConfigHandler {
	return &ConfigHandler{
		queries:       queries,
		timeWindowSec: timeWindowSec,
		minRSSI:       minRSSI,
		minAppVersion: minAppVersion,
		maxSessions:   maxSessions,
		killSwitch:    killSwitch,
	}
}

type configResponse struct {
	TimeWindowSec      int      `json:"time_window_sec"`
	MinRSSI            int      `json:"min_rssi"`
	MinAppVersion      int      `json:"min_app_version"`
	KillSwitch         bool     `json:"kill_switch"`
	Blocklist          []string `json:"blocklist"`
	MaxSessionsPerHour int      `json:"max_sessions_per_hour"`
}

func (h *ConfigHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// Fetch blocklist from DB
	blocklist, err := h.queries.GetBlocklist(ctx)
	if err != nil {
		blocklist = []string{}
	}
	if blocklist == nil {
		blocklist = []string{}
	}

	// Cache for 5 minutes
	w.Header().Set("Cache-Control", "public, max-age=300")

	writeJSON(w, http.StatusOK, configResponse{
		TimeWindowSec:      h.timeWindowSec,
		MinRSSI:            h.minRSSI,
		MinAppVersion:      h.minAppVersion,
		KillSwitch:         h.killSwitch,
		Blocklist:          blocklist,
		MaxSessionsPerHour: h.maxSessions,
	})
}
