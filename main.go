package main

import (
	"embed"

	"context"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "LXS Miner",
		Width:     560,
		Height:    860,
		MinWidth:  520,
		MinHeight: 720,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		// Match the website's dark background (#0b0d12).
		BackgroundColour: &options.RGBA{R: 11, G: 13, B: 18, A: 1},
		OnStartup:        app.startup,
		// Kill the spawned lxs child when the window closes. Without this the miner
		// keeps running headless — burning CPU in pool mode, and holding the Pebble
		// datadir lock in solo mode so the NEXT launch silently fails to mine.
		OnBeforeClose: func(ctx context.Context) bool { app.Stop(); return false },
		OnShutdown:    func(ctx context.Context) { app.Stop() },
		Mac: &mac.Options{
			TitleBar:             mac.TitleBarHiddenInset(),
			Appearance:           mac.NSAppearanceNameDarkAqua,
			WebviewIsTransparent: false,
			About: &mac.AboutInfo{
				Title:   "LXS Miner",
				Message: "Mine LXS — proof-of-work Layer-1.",
			},
		},
		Bind: []interface{}{app},
	})
	if err != nil {
		println("Error:", err.Error())
	}
}
