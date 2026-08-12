package server

import "github.com/google/wire"

var ServerProvider = wire.NewSet(NewServer, NewEngine, NewUploadEngine, NewUploadServer, NewUploadCleanupServer, NewSchedulerServer)
