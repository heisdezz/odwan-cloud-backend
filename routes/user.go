package routes

import (
	"net/http"

	"github.com/pocketbase/pocketbase/core"
)

func RegisterRoutes(se *core.ServeEvent) {
	registerTrashRoutes(se)
	router := se.Router
	router.GET("/api/test/connection", func(re *core.RequestEvent) error {
		re.Response.Header().Set("Cache-Control", "no-store")
		return re.String(http.StatusOK, "ok")
	})
	router.GET("/users", func(re *core.RequestEvent) error {
		return re.JSON(200, map[string]string{"hello": "world"})
	})
}
