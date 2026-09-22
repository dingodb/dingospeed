package config

import "testing"

func TestRegistrationAdvertisesManagementEndpoint(t *testing.T) {
	c := &Config{}
	c.Server.Repos = t.TempDir()
	c.Upload.Port = 18091
	c.Scheduler.Discovery.Host = "2001:db8::1"
	c.Scheduler.Discovery.Port = 8090
	management, download := c.RegistrationEndpoints()
	if management != "http://[2001:db8::1]:18091" || download != "http://[2001:db8::1]:8090" {
		t.Fatalf("%s %s", management, download)
	}
	r := c.Registration()
	r.NodeID = "node-one"
	r.ManagementURL = "https://proxy.example/manage"
	r.DownloadURL = "https://proxy.example/download"
	if err := c.SaveRegistration(r); err != nil {
		t.Fatal(err)
	}
	management, download = c.RegistrationEndpoints()
	if management != r.ManagementURL || download != r.DownloadURL {
		t.Fatal("explicit published endpoints lost")
	}
}
