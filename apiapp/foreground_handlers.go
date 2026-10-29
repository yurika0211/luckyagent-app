package server

import (
	"net/http"

	"github.com/yurika0211/luckyagent-api/resources"
)

func (s *Server) handleForegroundTasks(w http.ResponseWriter, r *http.Request) {
	s.resourceController().HandleForegroundTasks(w, r)
}

type foregroundTasksResponse = resources.ForegroundTasksResponse
