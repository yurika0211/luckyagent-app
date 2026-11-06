// Package qqofficial preserves host imports while implementation is assembled from independent edge modules.
package qqofficial

import app "github.com/yurika0211/luckyagent-app/channelapp/qqofficial"

type Adapter = app.Adapter
type Config = app.Config

var DefaultConfig = app.DefaultConfig

type Handler = app.Handler
type HandlerOptions = app.HandlerOptions
type NapCatProgressConfig = app.NapCatProgressConfig

var NewAdapter = app.NewAdapter
var NewHandler = app.NewHandler
var NewHandlerWithOptions = app.NewHandlerWithOptions
var NapCatHandlerOptions = app.NapCatHandlerOptions
var NapCatProgressFromConfigFile = app.NapCatProgressFromConfigFile

type Runtime = app.Runtime
