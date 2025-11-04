package query

type CreateCacheJobReq struct {
	Type         int32  `json:"type"`
	InstanceId   string `json:"instanceId"`
	Datatype     string `json:"datatype"`
	Org          string `json:"org"`
	Repo         string `json:"repo"`
	RepositoryId int64  `json:"repositoryId"`
}

type CacheJobQuery struct {
	Id         int32  `json:"id"`
	InstanceId string `json:"instanceId"`
	Type       int32  `json:"type"`
	Datatype   string `json:"datatype"`
	Org        string `json:"org"`
	Repo       string `json:"repo"`
	Token      string `json:"token"`
}

type ResumeCacheJobReq struct {
	Id         int64  `json:"id"`
	Type       int32  `json:"type"`
	InstanceId string `json:"instanceId"`
	Datatype   string `json:"datatype"`
	Org        string `json:"org"`
	Repo       string `json:"repo"`
	Token      string `json:"token"`
}

type JobStatusReq struct {
	Id         int64  `json:"id"`
	InstanceId string `json:"instanceId"`
}
