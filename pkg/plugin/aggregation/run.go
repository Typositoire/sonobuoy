/*
Copyright 2018 Heptio Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aggregation

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/heptio/sonobuoy/pkg/backplane/ca"
	"github.com/heptio/sonobuoy/pkg/plugin"
	"github.com/heptio/sonobuoy/pkg/plugin/driver/utils"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	"k8s.io/client-go/kubernetes"
)

const (
	annotationUpdateFreq = 5 * time.Second
	jitterFactor         = 1.2
)

// Run runs an aggregation server and gathers results, in accordance with the
// given sonobuoy configuration.
//
// Basic workflow:
//
// 1. Create the aggregator object (`aggr`) to keep track of results
// 2. Launch the HTTP server with the aggr's HandleHTTPResult function as the
//    callback
// 3. Run all the aggregation plugins, monitoring each one in a goroutine,
//    configuring them to send failure results through a shared channel
// 4. Hook the shared monitoring channel up to aggr's IngestResults() function
// 5. Block until aggr shows all results accounted for (results come in through
//    the HTTP callback), stopping the HTTP server on completion
func Run(client kubernetes.Interface, plugins []plugin.Interface, cfg plugin.AggregationConfig, namespace, outdir string) error {
	// Construct a list of things we'll need to dispatch
	if len(plugins) == 0 {
		logrus.Info("Skipping host data gathering: no plugins defined")
		return nil
	}

	// Get a list of nodes so the plugins can properly estimate what
	// results they'll give.
	// TODO: there are other places that iterate through the CoreV1.Nodes API
	// call, we should only do this in one place and cache it.
	nodes, err := client.CoreV1().Nodes().List(metav1.ListOptions{})
	if err != nil {
		return errors.WithStack(err)
	}

	// Find out what results we should expect for each of the plugins
	var expectedResults []plugin.ExpectedResult
	for _, p := range plugins {
		expectedResults = append(expectedResults, p.ExpectedResults(nodes.Items)...)
	}

	auth, err := ca.NewAuthority()
	if err != nil {
		return errors.Wrap(err, "couldn't make new certificate authority for plugin aggregator")
	}

	logrus.Infof("Starting server Expected Results: %v", expectedResults)

	// 1. Await results from each plugin
	aggr := NewAggregator(outdir+"/plugins", expectedResults)
	doneAggr := make(chan bool, 1)
	monitorCh := make(chan *plugin.Result, len(expectedResults))
	stopWaitCh := make(chan bool, 1)

	go func() {
		aggr.Wait(stopWaitCh)
		doneAggr <- true
	}()

	// AdvertiseAddress often has a port, split this off if so
	advertiseAddress := cfg.AdvertiseAddress
	if host, _, err := net.SplitHostPort(cfg.AdvertiseAddress); err == nil {
		advertiseAddress = host
	}

	tlsCfg, err := auth.MakeServerConfig(advertiseAddress)
	if err != nil {
		return errors.Wrap(err, "couldn't get a server certificate")
	}

	// 2. Launch the aggregation servers
	srv := &http.Server{
		Addr:      fmt.Sprintf("%s:%d", cfg.BindAddress, cfg.BindPort),
		Handler:   NewHandler(aggr.HandleHTTPResult),
		TLSConfig: tlsCfg,
	}

	doneServ := make(chan error)
	go func() {
		logrus.WithFields(logrus.Fields{
			"address": cfg.BindAddress,
			"port":    cfg.BindPort,
		}).Info("Starting aggregation server")
		doneServ <- srv.ListenAndServeTLS("", "")
	}()

	updater := newUpdater(expectedResults, namespace, client)
	ctx, cancel := context.WithCancel(context.TODO())
	pluginsdone := false
	defer func() {
		if pluginsdone == false {
			logrus.Info("Last update to annotations on exit")
			// This is the async exit cleanup function.
			// 1. Stop the annotation updater
			cancel()
			// 2. Try one last time to get an update out on exit
			if err := updater.Annotate(aggr.Results); err != nil {
				logrus.WithError(err).Info("couldn't annotate sonobuoy pod")
			}
		}
	}()

	// 3. Regularly annotate the Aggregator pod with the current run status
	logrus.Info("Starting annotation update routine")
	go func() {
		wait.JitterUntil(func() {
			pluginsdone = aggr.isComplete()
			if err := updater.Annotate(aggr.Results); err != nil {
				logrus.WithError(err).Info("couldn't annotate sonobuoy pod")
			}
			if pluginsdone {
				logrus.Info("All plugins have completed, status has been updated")
				cancel()
			}
		}, annotationUpdateFreq, jitterFactor, true, ctx.Done())
	}()

	// 4. Launch each plugin, to dispatch workers which submit the results back
	certs := map[string]*tls.Certificate{}
	for _, p := range plugins {
		cert, err := auth.ClientKeyPair(p.GetName())
		if err != nil {
			return errors.Wrapf(err, "couldn't make certificate for plugin %v", p.GetName())
		}
		certs[p.GetName()] = cert
	}

	for _, p := range plugins {
		logrus.WithField("plugin", p.GetName()).Info("Running plugin")
		if err = p.Run(client, cfg.AdvertiseAddress, certs[p.GetName()]); err != nil {
			err = errors.Wrapf(err, "error running plugin %v", p.GetName())
			logrus.Error(err)
			monitorCh <- utils.MakeErrorResult(p.GetResultType(), map[string]interface{}{"error": err.Error()}, "")
			continue
		}
		// Have the plugin monitor for errors
		go p.Monitor(client, nodes.Items, monitorCh)
	}
	// 5. Have the aggregator plumb results from each plugins' monitor function
	go aggr.IngestResults(monitorCh)

	// Give the plugins a chance to cleanup before a hard timeout occurs
	shutdownPlugins := time.After(time.Duration(cfg.TimeoutSeconds-plugin.GracefulShutdownPeriod) * time.Second)
	// Ensure we only wait for results for a certain time
	var timeout <-chan time.Time
	if cfg.TimeoutSeconds > 0 {
		timeout = time.After(time.Duration(cfg.TimeoutSeconds) * time.Second)
	}

	// 6. Wait for aggr to show that all results are accounted for
	for {
		select {
		case <-shutdownPlugins:
			Cleanup(client, plugins)
			logrus.Info("Gracefully shutting down plugins due to timeout.")
		case <-timeout:
			srv.Close()
			stopWaitCh <- true
			return errors.Errorf("timed out waiting for plugins, shutting down HTTP server")
		case err := <-doneServ:
			stopWaitCh <- true
			return err
		case <-doneAggr:
			return nil
		}
	}
}

// Cleanup calls cleanup on all plugins
func Cleanup(client kubernetes.Interface, plugins []plugin.Interface) {
	// Cleanup after each plugin
	for _, p := range plugins {
		p.Cleanup(client)
	}
}
