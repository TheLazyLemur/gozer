// Command demo is a sample gozer app: go generate && go build gives one binary.
package main

//go:generate go run github.com/TheLazyLemur/gozer/cmd/gozer

import (
	"embed"
	"io/fs"
	"log"
	"os"

	"github.com/TheLazyLemur/gozer"
)

//go:embed static
var static embed.FS

func main() {
	app := gozer.New()
	app.Title = "gozer demo"
	app.Head = `<link rel="stylesheet" href="/static/app.css"><script src="/static/app.js"></script>`
	app.Root = gozer.Factory[App]()
	sub, err := fs.Sub(static, "static")
	if err != nil {
		log.Fatal(err)
	}
	app.Static("/static/", sub)
	gozer.Provide(app, NewWeatherService())

	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}
	log.Fatal(app.ListenAndServe(addr))
}
