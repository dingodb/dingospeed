package downloader

import "dingo-hfmirror/pkg/common"

var RespNoticeMap = common.NewSafeMap[string, *Broadcaster]()
