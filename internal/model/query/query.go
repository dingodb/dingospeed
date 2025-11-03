package query

type CacheJobQuery struct {
	Id         int32  `json:"id"`
	InstanceId string `json:"instanceId"`
	Type       int32  `json:"type"`
	Datatype   string `json:"datatype"`
	Org        string `json:"org"`
	Repo       string `json:"repo"`
	Token      string `json:"token"`
}

type JobStatus struct {
	Id         int64  `json:"id"`
	InstanceId string `json:"instanceId"`
	Status     int32  `json:"status"`
	ErrorMsg   string `json:"errorMsg"`
}
