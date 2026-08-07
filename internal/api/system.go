package api

import (
	"encoding/json"
	"net/http"
	"runtime"
	"syscall"
	"time"

	"github.com/rousoftware/asgard/internal/httpx"
)

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	projects, err := s.Store.ListProjects(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	host := s.Docker.Host(r.Context())
	var totalServices, running int
	var memoryUsed int64
	var cpu float64
	for _, project := range projects {
		for _, svc := range project.Services {
			totalServices++
			if svc.Runtime != nil && svc.Runtime.State == "running" {
				running++
			}
			if svc.Metrics != nil {
				memoryUsed += svc.Metrics.MemoryBytes
				cpu += svc.Metrics.CPUPercent
			}
		}
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"projects": projects, "summary": map[string]any{"projects": len(projects), "services": totalServices, "running": running, "cpuPercent": cpu, "memoryBytes": memoryUsed}, "host": host, "disk": diskUsage(s.Config.DataDir), "generatedAt": time.Now().UTC()})
}

func (s *Server) system(w http.ResponseWriter, r *http.Request) {
	host := s.Docker.Host(r.Context())
	httpx.JSON(w, http.StatusOK, map[string]any{"host": host, "disk": diskUsage(s.Config.DataDir), "goVersion": runtime.Version(), "publicUrl": s.Config.PublicURL, "domain": s.Config.Domain, "timezone": s.Config.Timezone.String(), "mcpUrl": s.Config.PublicURL + "/mcp", "backupPath": s.Config.BackupsDir})
}

func diskUsage(path string) map[string]any {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return map[string]any{"available": false, "error": err.Error()}
	}
	total := int64(stat.Blocks) * int64(stat.Bsize)
	free := int64(stat.Bavail) * int64(stat.Bsize)
	return map[string]any{"available": true, "totalBytes": total, "freeBytes": free, "usedBytes": total - free, "usedPercent": float64(total-free) / float64(total) * 100}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpx.Error(w, http.StatusNotImplemented, "stream_unavailable", "Streaming is unavailable.")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		ops, err := s.Store.ListOperations(r.Context(), 20)
		if err == nil {
			bytes, _ := json.Marshal(map[string]any{"operations": ops, "at": time.Now().UTC()})
			_, _ = w.Write([]byte("event: snapshot\ndata: " + string(bytes) + "\n\n"))
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}
