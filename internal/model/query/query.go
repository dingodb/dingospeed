package query

type CreateCacheJobReq struct {
	Org          string `json:"org,omitempty"`
	OrgRepo      string `json:"orgRepo,omitempty"`
	Type         int32  `json:"type"`
	InstanceId   string `json:"instanceId"`
	Datatype     string `json:"datatype"`
	Namespace    string `json:"namespace"`
	Repo         string `json:"repo"`
	RepositoryId int64  `json:"repositoryId"`
}

type ResumeCacheJobReq struct {
	Org         string `json:"org,omitempty"`
	Id          int64  `json:"id"`
	Type        int32  `json:"type"`
	InstanceId  string `json:"instanceId"`
	Datatype    string `json:"datatype"`
	Namespace   string `json:"namespace"`
	Repo        string `json:"repo"`
	Token       string `json:"token"`
	UsedStorage int64  `json:"usedStorage"`
}

type JobStatusReq struct {
	Intent     string `json:"intent,omitempty"`
	Id         int64  `json:"id"`
	InstanceId string `json:"instanceId"`
}

type RealtimeReq struct {
	CacheJobIds []int64 `json:"cacheJobIds"`
}

type RealtimeResp struct {
	CacheJobId   int64   `json:"cacheJobId"`
	StockSpeed   string  `json:"stockSpeed"`
	StockProcess float32 `json:"stockProcess"`
}
