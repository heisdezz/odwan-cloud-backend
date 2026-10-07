package main

import (
	appinit "go-test/init"
	"go-test/routes"
	"go-test/s3"
	"log"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

func main() {
	config, err := s3.LoadConfig("")
	if err != nil {
		log.Fatal(err)
	}
	app := pocketbase.New()
	app.OnBootstrap().BindFunc(appinit.Initialize)
	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		// registers new "GET /hello" route
		//

		se.Router.GET("/hello", func(re *core.RequestEvent) error {
			return re.JSON(200, map[string]string{"hello": "world"})
		})
		routes.RegisterRoutes(se)
		if err := routes.RegisterStorageRoutes(se, config); err != nil {
			return err
		}
		return se.Next()
	})

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
