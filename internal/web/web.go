// Package web serves the JSON API, the SSE stream and the embedded UI.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"portwake/internal/config"
	"portwake/internal/events"
	"portwake/internal/project"
	"portwake/internal/store"
)

//go:embed static
var static embed.FS

// Backend is what the web layer needs from the application.
type Backend interface {
	Projects() []config.Project
	// UpdateProjects atomically transforms and applies the configuration.
	UpdateProjects(func([]config.Project) ([]config.Project, error)) error
	Snapshots() []project.Snapshot
	StartProject(name string) error
	StopProject(name string) error
	ProjectLogs(ctx context.Context, name string, lines int) (string, error)
	Events(store.EventQuery) ([]events.Event, error)
	Stats(project string, from, to time.Time) ([]store.ProjectStats, error)
	Bus() *events.Bus
}

func init() { gin.SetMode(gin.ReleaseMode) }

func Handler(b Backend) http.Handler {
	h := &handler{b: b}
	r := gin.New()
	r.Use(gin.Recovery())

	api := r.Group("/api")
	api.GET("/status", h.status)
	api.GET("/events", h.events)
	api.GET("/stats", h.stats)
	api.GET("/stream", h.stream)
	api.GET("/projects", h.listProjects)
	api.POST("/projects", h.createProject)
	api.PUT("/projects/:name", h.updateProject)
	api.DELETE("/projects/:name", h.deleteProject)
	api.POST("/projects/:name/start", h.startProject)
	api.POST("/projects/:name/stop", h.stopProject)
	api.GET("/projects/:name/logs", h.projectLogs)
	api.GET("/config.yaml", h.exportYAML)
	api.PUT("/config.yaml", h.importYAML)

	sub, _ := fs.Sub(static, "static")
	files := http.FileServerFS(sub)
	r.NoRoute(func(c *gin.Context) {
		if c.Request.Method != http.MethodGet || strings.HasPrefix(c.Request.URL.Path, "/api/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		files.ServeHTTP(c.Writer, c.Request)
	})
	return r
}

type handler struct{ b Backend }

func writeErr(c *gin.Context, code int, err error) {
	c.JSON(code, gin.H{"error": err.Error()})
}

func (h *handler) status(c *gin.Context) {
	c.JSON(http.StatusOK, h.b.Snapshots())
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.UnixMilli(n)
	}
	return time.Time{}
}

func (h *handler) events(c *gin.Context) {
	eq := store.EventQuery{
		Project: c.Query("project"),
		Since:   parseTime(c.Query("since")),
		Until:   parseTime(c.Query("until")),
	}
	if t := c.Query("type"); t != "" {
		eq.Types = strings.Split(t, ",")
	}
	eq.Limit, _ = strconv.Atoi(c.Query("limit"))
	eq.BeforeID, _ = strconv.ParseInt(c.Query("before_id"), 10, 64)
	evs, err := h.b.Events(eq)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, evs)
}

func (h *handler) stats(c *gin.Context) {
	from, to := parseTime(c.Query("from")), parseTime(c.Query("to"))
	if to.IsZero() {
		to = time.Now().Add(time.Hour)
	}
	if from.IsZero() {
		from = to.Add(-30 * 24 * time.Hour)
	}
	st, err := h.b.Stats(c.Query("project"), from, to)
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, st)
}

// stream pushes every bus event as an SSE "data:" line.
func (h *handler) stream(c *gin.Context) {
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	ch, cancel := h.b.Bus().Subscribe(256)
	defer cancel()
	fmt.Fprint(w, ": connected\n\n")
	w.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
		case e, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(e)
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
		}
		w.Flush()
	}
}

func (h *handler) listProjects(c *gin.Context) {
	c.JSON(http.StatusOK, h.b.Projects())
}

func decodeProject(c *gin.Context) (config.Project, error) {
	var p config.Project
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return p, fmt.Errorf("invalid project JSON: %w", err)
	}
	return p, nil
}

// errStatus maps backend errors to HTTP status codes.
func errStatus(err error) int {
	switch {
	case errors.Is(err, project.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, config.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, project.ErrBusy):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// applyConfig runs a configuration change and reports whether it succeeded,
// writing the error response when it did not.
func (h *handler) applyConfig(c *gin.Context, fn func([]config.Project) ([]config.Project, error)) bool {
	if err := h.b.UpdateProjects(fn); err != nil {
		writeErr(c, errStatus(err), err)
		return false
	}
	return true
}

func (h *handler) createProject(c *gin.Context) {
	p, err := decodeProject(c)
	if err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	if h.applyConfig(c, func(ps []config.Project) ([]config.Project, error) { return append(ps, p), nil }) {
		c.JSON(http.StatusCreated, h.b.Projects())
	}
}

func indexOf(ps []config.Project, name string) int {
	return slices.IndexFunc(ps, func(p config.Project) bool { return p.Name == name })
}

func (h *handler) updateProject(c *gin.Context) {
	p, err := decodeProject(c)
	if err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	name := c.Param("name")
	ok := h.applyConfig(c, func(ps []config.Project) ([]config.Project, error) {
		i := indexOf(ps, name)
		if i < 0 {
			return nil, project.ErrNotFound
		}
		ps[i] = p
		return ps, nil
	})
	if ok {
		c.JSON(http.StatusOK, h.b.Projects())
	}
}

func (h *handler) deleteProject(c *gin.Context) {
	name := c.Param("name")
	ok := h.applyConfig(c, func(ps []config.Project) ([]config.Project, error) {
		i := indexOf(ps, name)
		if i < 0 {
			return nil, project.ErrNotFound
		}
		return slices.Delete(ps, i, i+1), nil
	})
	if ok {
		c.Status(http.StatusNoContent)
	}
}

func respondAccepted(c *gin.Context, err error) {
	if err != nil {
		writeErr(c, errStatus(err), err)
		return
	}
	c.Status(http.StatusAccepted)
}

func (h *handler) startProject(c *gin.Context) {
	respondAccepted(c, h.b.StartProject(c.Param("name")))
}

func (h *handler) stopProject(c *gin.Context) {
	respondAccepted(c, h.b.StopProject(c.Param("name")))
}

// projectLogs returns the tail of a project's output as plain text.
func (h *handler) projectLogs(c *gin.Context) {
	lines, _ := strconv.Atoi(c.DefaultQuery("lines", "200"))
	if lines <= 0 {
		lines = 200
	}
	if lines > 2000 {
		lines = 2000
	}
	out, err := h.b.ProjectLogs(c.Request.Context(), c.Param("name"), lines)
	if err != nil {
		writeErr(c, errStatus(err), err)
		return
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(out))
}

func (h *handler) exportYAML(c *gin.Context) {
	b, err := config.MarshalYAML(h.b.Projects())
	if err != nil {
		writeErr(c, http.StatusInternalServerError, err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="portwake.yaml"`)
	c.Data(http.StatusOK, "application/yaml", b)
}

func (h *handler) importYAML(c *gin.Context) {
	b, err := io.ReadAll(io.LimitReader(c.Request.Body, 4<<20))
	if err != nil {
		writeErr(c, http.StatusBadRequest, err)
		return
	}
	ps, err := config.UnmarshalYAML(b)
	if err != nil {
		writeErr(c, http.StatusBadRequest, fmt.Errorf("invalid YAML: %w", err))
		return
	}
	if h.applyConfig(c, func([]config.Project) ([]config.Project, error) { return ps, nil }) {
		c.JSON(http.StatusOK, h.b.Projects())
	}
}
