package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Registration struct {
	Enabled       bool   `json:"enabled"`
	NodeID        string `json:"nodeId"`
	Address       string `json:"address"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	ManagementURL string `json:"managementUrl,omitempty"`
	DownloadURL   string `json:"downloadUrl,omitempty"`
}
type RegistrationStatus struct {
	State     string    `json:"state"`
	Error     string    `json:"error,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

func ValidateRegistration(r Registration) error {
	for _, raw := range []string{r.ManagementURL, r.DownloadURL} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("invalid advertised URL")
		}
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`).MatchString(r.NodeID) {
		return errors.New("invalid immutable node ID")
	}
	if !r.Enabled {
		return nil
	}
	host, port, err := net.SplitHostPort(r.Address)
	p, e := strconv.Atoi(port)
	if err != nil || e != nil || host == "" || p < 1 || p > 65535 || strings.ContainsAny(host, "/\\ \t\r\n") {
		return errors.New("scheduler address must be host:port")
	}
	if r.Host == "" || strings.ContainsAny(r.Host, "/\\ \t\r\n") || r.Port < 1 || r.Port > 65535 {
		return errors.New("invalid advertised host/port")
	}
	return nil
}
func (c *Config) registrationPath() string {
	return filepath.Join(c.Repos(), ".scheduler-registration.json")
}
func (c *Config) Registration() Registration {
	c.registrationOnce.Do(func() {
		r := Registration{Enabled: c.Scheduler.OriginMode == "cluster" || c.Scheduler.Mode == "cluster", NodeID: c.Scheduler.Discovery.InstanceId, Address: c.Scheduler.Addr, Host: c.Scheduler.Discovery.Host, Port: c.Scheduler.Discovery.Port, ManagementURL: c.Upload.AdvertiseURL}
		b, err := os.ReadFile(c.registrationPath())
		if err == nil {
			err = json.Unmarshal(b, &r)
			if err == nil {
				err = ValidateRegistration(r)
			}
			c.registrationPersisted = err == nil
		}
		if err != nil && !os.IsNotExist(err) {
			c.registrationErr = err
			r.Enabled = false
		}
		c.registration = &r
	})
	c.mu.RLock()
	defer c.mu.RUnlock()
	return *c.registration
}
func (c *Config) SaveRegistration(r Registration) error {
	if err := ValidateRegistration(r); err != nil {
		return err
	}
	c.Registration()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.registrationErr != nil {
		return fmt.Errorf("cannot replace unreadable registration: %w", c.registrationErr)
	}
	if c.registration.NodeID != "" && c.registration.NodeID != r.NodeID {
		return errors.New("node ID is already bound and cannot change")
	}
	if *c.registration == r && c.registrationPersisted {
		return nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(c.Repos(), ".scheduler-registration-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), c.registrationPath()); err != nil {
		return err
	}
	c.registration = &r
	c.registrationPersisted = true
	c.registrationStatus = RegistrationStatus{State: "connecting"}
	c.Scheduler.Mode = "standalone"
	return nil
}
func (c *Config) SetRegistrationStatus(r Registration, state string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.registration == nil || *c.registration != r {
		return
	}
	s := RegistrationStatus{State: state, CheckedAt: time.Now().UTC()}
	if err != nil {
		s.Error = err.Error()
	}
	c.registrationStatus = s
	if state == "connected" {
		c.Scheduler.Mode = "cluster"
	} else {
		c.Scheduler.Mode = "standalone"
	}
}
func (c *Config) RegistrationState() RegistrationStatus {
	r := c.Registration()
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.registrationErr != nil {
		return RegistrationStatus{State: "error", Error: c.registrationErr.Error()}
	}
	if !r.Enabled {
		return RegistrationStatus{State: "disabled"}
	}
	s := c.registrationStatus
	if s.State == "" {
		s.State = "connecting"
	}
	return s
}
func (c *Config) SetSchedulerID(id int32) { c.mu.Lock(); defer c.mu.Unlock(); c.Id = id }
func (c *Config) SchedulerID() int32      { c.mu.RLock(); defer c.mu.RUnlock(); return c.Id }

// Explicit URLs support proxies and published container ports. The default uses
// the advertised node host, never the upload listen address (often 0.0.0.0).
func (c *Config) RegistrationEndpoints() (string, string) {
	r := c.Registration()
	management, download := r.ManagementURL, r.DownloadURL
	if management == "" {
		management = c.Upload.AdvertiseURL
	}
	if management == "" && r.Host != "" {
		port := c.Upload.Port
		if port == 0 {
			port = 8091
		}
		management = "http://" + net.JoinHostPort(r.Host, strconv.Itoa(port))
	}
	if download == "" && r.Host != "" {
		download = "http://" + net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
	}
	return management, download
}
