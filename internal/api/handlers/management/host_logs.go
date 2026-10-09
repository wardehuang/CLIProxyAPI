package management

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// HandleHostManagementLogs exposes only the read-only log handlers to authorized plugins.
func (h *Handler) HandleHostManagementLogs(ctx context.Context, request pluginapi.HostManagementLogsRequest) (pluginapi.HostManagementLogsResponse, error) {
	var handler func(*gin.Context)
	params := gin.Params{}
	switch request.Operation {
	case pluginapi.HostManagementLogsOperationStatus:
		handler = func(c *gin.Context) {
			if h.cfg == nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"logging_to_file": h.cfg.LoggingToFile,
				"request_log":     h.cfg.RequestLog,
			})
		}
	case pluginapi.HostManagementLogsOperationLogs:
		handler = h.GetLogs
	case pluginapi.HostManagementLogsOperationServerFiles:
		handler = h.listServerLogFiles
	case pluginapi.HostManagementLogsOperationServerFile:
		handler = h.downloadServerLogFile
		params = append(params, gin.Param{Key: "name", Value: request.Name})
	case pluginapi.HostManagementLogsOperationRequestFiles:
		handler = h.listRequestLogFiles
	case pluginapi.HostManagementLogsOperationRequestLogFile:
		handler = h.downloadRequestLogFile
		params = append(params, gin.Param{Key: "name", Value: request.Name})
	case pluginapi.HostManagementLogsOperationRequestFile:
		handler = h.GetRequestLogByID
		params = append(params, gin.Param{Key: "id", Value: request.ID})
	default:
		return pluginapi.HostManagementLogsResponse{}, http.ErrNotSupported
	}

	target := "/"
	if query := request.Query.Encode(); query != "" {
		target += "?" + query
	}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx)
	ginContext.Params = params
	handler(ginContext)

	response := recorder.Result()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return pluginapi.HostManagementLogsResponse{}, err
	}
	return pluginapi.HostManagementLogsResponse{
		StatusCode: response.StatusCode,
		Headers:    response.Header.Clone(),
		Body:       body,
	}, nil
}

type serverLogFile struct {
	Name string `json:"name"`
	Time string `json:"time"`
	Size int64  `json:"size"`
}

type serverLogFileListItem struct {
	serverLogFile
	modTime time.Time
}

func (h *Handler) listServerLogFiles(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	logDir := h.logDirectory()
	if strings.TrimSpace(logDir) == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "log directory not configured"})
		return
	}
	paths, err := h.collectLogFiles(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			writeServerLogFiles(c, []serverLogFile{}, c.Query("page"), c.Query("page_size"))
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list server log files"})
		return
	}

	items := make([]serverLogFileListItem, 0, len(paths))
	for _, path := range paths {
		info, errInfo := os.Stat(path)
		if errInfo != nil || info.IsDir() {
			continue
		}
		modified := info.ModTime()
		items = append(items, serverLogFileListItem{
			serverLogFile: serverLogFile{
				Name: info.Name(),
				Time: modified.Format(time.RFC3339),
				Size: info.Size(),
			},
			modTime: modified,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].modTime.After(items[j].modTime) })

	files := make([]serverLogFile, 0, len(items))
	for _, item := range items {
		files = append(files, item.serverLogFile)
	}
	writeServerLogFiles(c, files, c.Query("page"), c.Query("page_size"))
}

func writeServerLogFiles(c *gin.Context, files []serverLogFile, rawPage, rawPageSize string) {
	total := len(files)
	page := positiveLogQueryInt(rawPage, 1)
	pageSize := positiveLogQueryInt(rawPageSize, 12)
	if pageSize > 200 {
		pageSize = 200
	}
	totalPages := 1
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	c.JSON(http.StatusOK, gin.H{
		"files":       files[start:end],
		"page":        page,
		"page_size":   pageSize,
		"total":       total,
		"total_pages": totalPages,
	})
}

func positiveLogQueryInt(raw string, fallback int) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func (h *Handler) downloadServerLogFile(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	logDir := h.logDirectory()
	if strings.TrimSpace(logDir) == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "log directory not configured"})
		return
	}
	path, errPath := safeLogFilePath(logDir, c.Param("name"))
	if errPath != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid log file name"})
		return
	}
	info, errStat := os.Stat(path)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			c.JSON(http.StatusNotFound, gin.H{"error": "log file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read server log file"})
		return
	}
	if info.IsDir() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid log file"})
		return
	}
	body, errRead := os.ReadFile(path)
	if errRead != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read server log file"})
		return
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", body)
}

type requestLogFile struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
}

type requestLogFileListItem struct {
	requestLogFile
	modTime time.Time
}

func (h *Handler) listRequestLogFiles(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	logDir := h.logDirectory()
	if strings.TrimSpace(logDir) == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "log directory not configured"})
		return
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		if os.IsNotExist(err) {
			writeRequestLogFiles(c, []requestLogFile{}, c.Query("page"), c.Query("page_size"))
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list request log files"})
		return
	}
	items := make([]requestLogFileListItem, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !isRequestLogFileName(entry.Name()) {
			continue
		}
		info, errInfo := entry.Info()
		if errInfo != nil {
			continue
		}
		modified := info.ModTime()
		items = append(items, requestLogFileListItem{
			requestLogFile: requestLogFile{Name: entry.Name(), Size: info.Size(), Modified: modified.Unix()},
			modTime:        modified,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].modTime.Equal(items[j].modTime) {
			return items[i].Name > items[j].Name
		}
		return items[i].modTime.After(items[j].modTime)
	})
	files := make([]requestLogFile, 0, len(items))
	for _, item := range items {
		files = append(files, item.requestLogFile)
	}
	writeRequestLogFiles(c, files, c.Query("page"), c.Query("page_size"))
}

func writeRequestLogFiles(c *gin.Context, files []requestLogFile, rawPage, rawPageSize string) {
	total := len(files)
	page := positiveLogQueryInt(rawPage, 1)
	pageSize := positiveLogQueryInt(rawPageSize, 12)
	if pageSize > 200 {
		pageSize = 200
	}
	totalPages := 1
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	c.JSON(http.StatusOK, gin.H{
		"files":       files[start:end],
		"page":        page,
		"page_size":   pageSize,
		"total":       total,
		"total_pages": totalPages,
	})
}

func isRequestLogFileName(name string) bool {
	return name != defaultLogFileName &&
		!strings.HasPrefix(name, defaultLogFileName+".") &&
		filepath.Base(name) == name &&
		!strings.ContainsAny(name, `/\\`) &&
		filepath.Ext(name) == ".log" &&
		parseLogMetadata(name).hasTime
}

func (h *Handler) downloadRequestLogFile(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "configuration unavailable"})
		return
	}
	name := c.Param("name")
	if !isRequestLogFileName(name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request log file name"})
		return
	}
	logDir := h.logDirectory()
	if strings.TrimSpace(logDir) == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "log directory not configured"})
		return
	}
	path := filepath.Join(logDir, name)
	info, errStat := os.Lstat(path)
	if errStat != nil {
		if os.IsNotExist(errStat) {
			c.JSON(http.StatusNotFound, gin.H{"error": "request log file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read request log file"})
		return
	}
	if info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request log file"})
		return
	}
	body, errRead := os.ReadFile(path)
	if errRead != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read request log file"})
		return
	}
	c.Data(http.StatusOK, "text/plain; charset=utf-8", body)
}
