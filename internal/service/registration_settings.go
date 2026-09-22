package service

import (
	"dingospeed/pkg/common"
	"dingospeed/pkg/config"
	"errors"
	"sync"
)

var registrationAdmission sync.Mutex
var registrationCachePool *common.Pool // wired before servers start

func SaveSchedulerRegistration(r config.Registration) error {
	registrationAdmission.Lock()
	defer registrationAdmission.Unlock()
	cacheStateMu.Lock()
	hasExecutions := len(cacheExecutions) > 0
	cacheStateMu.Unlock()
	if config.SysConfig.Registration() != r && (hasExecutions || (registrationCachePool != nil && registrationCachePool.ActiveCount() > 0)) {
		return errors.New("pause or cancel active cache jobs before changing scheduler registration")
	}
	return config.SysConfig.SaveRegistration(r)
}
