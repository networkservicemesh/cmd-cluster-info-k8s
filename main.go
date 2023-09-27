// Copyright (c) 2022-2023 Cisco and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/edwarnicke/serialize"
	"github.com/kelseyhightower/envconfig"
	"gopkg.in/yaml.v2"

	"github.com/sirupsen/logrus"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/log/logruslogger"
	"github.com/networkservicemesh/sdk/pkg/tools/opentelemetry"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	aboutv1alpha1 "k8s.io/clusterproperty/api/v1alpha1"
)

// Config represents the configuration for cmd-map-ip-k8s application
type Config struct {
	LogLevel              string            `default:"INFO" desc:"Log level" split_words:"true"`
	ConfigmapName         string            `default:"cluster-info" desc:"Configmap to write" split_words:"true"`
	Namespace             string            `default:"default" desc:"Namespace where app is deployed" split_words:"true"`
	OpenTelemetryEndpoint string            `default:"otel-collector.observability.svc.cluster.local:4317" desc:"OpenTelemetry Collector Endpoint"`
	MetricsExportInterval time.Duration     `default:"10s" desc:"interval between mertics exports" split_words:"true"`
	TranslationMap        map[string]string `default:"id.k8s.io:clusterName" desc:"Replaces cluster property name to another if it's presented the map" split_words:"true"`
	FileName              string            `default:"config.yaml" desc:"Name of output data" split_words:"true"`
}

func main() {
	// ********************************************************************************
	// Configure signal handling context
	// ********************************************************************************
	ctx, cancel := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		// More Linux signals here
		syscall.SIGHUP,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
	defer cancel()

	// ********************************************************************************
	// Setup logger
	// ********************************************************************************
	log.EnableTracing(true)
	logrus.Info("Starting cluster-info-k8s...")
	logrus.SetFormatter(&nested.Formatter{})
	ctx = log.WithLog(ctx, logruslogger.New(ctx, map[string]interface{}{"cmd": os.Args[:1]}))

	logger := log.FromContext(ctx)

	// ********************************************************************************
	// Get config from environment
	// ********************************************************************************
	conf := &Config{}
	if err := envconfig.Usage("nsm", conf); err != nil {
		logger.Fatal(err)
	}
	if err := envconfig.Process("nsm", conf); err != nil {
		logger.Fatalf("error processing rootConf from env: %+v", err)
	}

	level, err := logrus.ParseLevel(conf.LogLevel)
	if err != nil {
		logrus.Fatalf("invalid log level %s", conf.LogLevel)
	}
	logrus.SetLevel(level)

	// ********************************************************************************
	// Configure Open Telemetry
	// ********************************************************************************
	if opentelemetry.IsEnabled() {
		collectorAddress := conf.OpenTelemetryEndpoint
		spanExporter := opentelemetry.InitSpanExporter(ctx, collectorAddress)
		metricExporter := opentelemetry.InitOPTLMetricExporter(ctx, collectorAddress, conf.MetricsExportInterval)
		o := opentelemetry.Init(ctx, spanExporter, metricExporter, "map-ip-k8s")
		defer func() {
			if err = o.Close(); err != nil {
				logger.Error(err.Error())
			}
		}()
	}

	// ********************************************************************************
	// Create client-go
	// ********************************************************************************
	RESTConfig, err := rest.InClusterConfig()
	if err != nil {
		logger.Fatal(err.Error())
	}
	c, err := kubernetes.NewForConfig(RESTConfig)
	if err != nil {
		logger.Fatal(err.Error())
	}

	if err = aboutv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		logger.Fatal(err.Error())
	}

	aboutapiRESTConfig := *RESTConfig
	aboutapiRESTConfig.ContentConfig.GroupVersion = &schema.GroupVersion{Group: aboutv1alpha1.GroupVersion.Group, Version: aboutv1alpha1.GroupVersion.Version}
	aboutapiRESTConfig.APIPath = "/apis"
	aboutapiRESTConfig.NegotiatedSerializer = serializer.NewCodecFactory(scheme.Scheme)
	aboutapiRESTConfig.UserAgent = rest.DefaultKubernetesUserAgent()

	aboutapiRESTClient, err := rest.UnversionedRESTClientFor(&aboutapiRESTConfig)
	if err != nil {
		logger.Fatal(err.Error())
	}

	var updater = configMapUpdater{
		ctx: ctx,
		configMapMeta: metav1.ObjectMeta{
			Namespace: conf.Namespace,
			Name:      conf.ConfigmapName,
		},
		fileName:  conf.FileName,
		k8sClient: c,
		logger:    logger.WithField("cluster-info-k8s", "configMapUpdater"),
	}

	for ; ctx.Err() == nil; time.Sleep(time.Second) {
		var clusterPropertyList aboutv1alpha1.ClusterPropertyList
		err = aboutapiRESTClient.Get().Resource("clusterproperties").Do(ctx).Into(&clusterPropertyList)

		if err != nil {
			logger.Warnf("got an error during REST call %v", err.Error())
		}

		var properties = make(map[string]string)
		for i := 0; i < len(clusterPropertyList.Items); i++ {
			var property = &clusterPropertyList.Items[i]
			var k, v = property.Name, property.Spec.Value
			if translation := conf.TranslationMap[k]; translation != "" {
				k = translation
			}
			properties[k] = v
		}
		updater.ScheduleSoftUpdate(properties)
	}

	// ********************************************************************************
	// Initialize goroutines for writing ips map
	// ********************************************************************************
	<-ctx.Done()
}

type configMapUpdater struct {
	ctx           context.Context
	configMapMeta metav1.ObjectMeta
	fileName      string
	executor      serialize.Executor
	k8sClient     *kubernetes.Clientset
	logger        log.Logger
}

func (c *configMapUpdater) ScheduleSoftUpdate(change map[string]string) {
	c.executor.AsyncExec(func() { c.SoftUpdate(change) })
}

func (c *configMapUpdater) SoftUpdate(change map[string]string) {
	var ctx, cancel = context.WithTimeout(c.ctx, time.Second)
	defer cancel()

	resp, err := c.k8sClient.CoreV1().ConfigMaps(c.configMapMeta.Namespace).Get(ctx, c.configMapMeta.Name, metav1.GetOptions{})
	if err != nil {
		c.logger.Errorf("got an error during get configmap: %+v, err: %v", c.configMapMeta, err.Error())
		return
	}
	if resp.Data == nil {
		resp.Data = make(map[string]string)
	}
	var fileData = resp.Data[c.fileName]
	var configMapFileValues = make(map[string]string)
	_ = yaml.Unmarshal([]byte(fileData), &configMapFileValues)
	var hasDiff = false
	for k, v := range change {
		if configMapFileValues[k] != v {
			configMapFileValues[k] = v
			hasDiff = true
		}
	}

	if !hasDiff {
		return
	}

	var configMapFileValuesRaw, _ = yaml.Marshal(&configMapFileValues)

	resp.Data[c.fileName] = string(configMapFileValuesRaw)

	_, err = c.k8sClient.CoreV1().ConfigMaps(c.configMapMeta.Namespace).Update(ctx, resp, metav1.UpdateOptions{})

	if err != nil {
		c.logger.Errorf("got an error during update configmap: %+v, err: %v", c.configMapMeta, err.Error())
	}
}
