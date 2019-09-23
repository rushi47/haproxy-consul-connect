package main

import (
	"fmt"
	"io/ioutil"
	"testing"

	"net/http"

	"github.com/criteo/haproxy-consul-connect/lib"
	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/require"
)

func TestSetup(t *testing.T) {
	sd := lib.NewShutdown()
	defer func() {
		sd.Shutdown("test end")
		sd.Wait()
	}()

	client := startAgent(t, sd)

	csd, _, upstreamPorts := startConnectService(t, sd, client, &api.AgentServiceRegistration{
		Name: "source",
		ID:   "source-1",

		Connect: &api.AgentServiceConnect{
			SidecarService: &api.AgentServiceRegistration{
				Proxy: &api.AgentServiceConnectProxyConfig{
					Upstreams: []api.Upstream{
						api.Upstream{
							DestinationName: "target",
						},
					},
				},
			},
		},
	})

	tsd, servicePort, _ := startConnectService(t, sd, client, &api.AgentServiceRegistration{
		Name: "target",
		ID:   "target-1",

		Connect: &api.AgentServiceConnect{
			SidecarService: &api.AgentServiceRegistration{
				Proxy: &api.AgentServiceConnectProxyConfig{},
			},
		},
	})

	startServer(t, sd, servicePort, "hello connect")

	wait(csd, tsd)

	res, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d", upstreamPorts["target"]))
	require.NoError(t, err)
	require.Equal(t, 200, res.StatusCode)

	body, err := ioutil.ReadAll(res.Body)
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, "hello connect", string(body))
}
