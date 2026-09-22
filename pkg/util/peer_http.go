package util

import (
	"context"
	"dingospeed/pkg/config"
	"dingospeed/pkg/consts"
	"dingospeed/pkg/transfersettings"
	"net/http"
)

// GetPeerStream reads from the selected peer without following redirects.
func GetPeerStream(domain, uri string, headers map[string]string, consume func(*http.Response) error) error {
	return GetPeerStreamContext(context.Background(), domain, uri, headers, consume)
}

func GetPeerStreamContext(ctx context.Context, domain, uri string, headers map[string]string, consume func(*http.Response) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, domain+uri, nil)
	if err != nil {
		return err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	req.Header.Set(consts.RequestSourceInner, "1")
	client := &http.Client{Timeout: config.SysConfig.GetReqTimeOut(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := transfersettings.Do("peer", client, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return consume(resp)
}
