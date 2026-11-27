package websocket

import transport "github.com/yurika0211/luckyagent-api/websocket"

type persistedRun = transport.RunRecord

var newRunStore = func(root string) *runStore { return &runStore{RunStore: transport.NewRunStore(root), root: root} }
var errRunConflict = transport.ErrRunConflict

type runStore struct {
	*transport.RunStore
	root string
}
