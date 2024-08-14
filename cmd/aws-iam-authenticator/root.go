/*
Copyright 2017 by the contributors.

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

package main

import (
	"errors"
	"fmt"
	"os"

	"sigs.k8s.io/aws-iam-authenticator/pkg/config"
	"sigs.k8s.io/aws-iam-authenticator/pkg/mapper"

	"github.com/aws/aws-sdk-go/aws/endpoints"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/component-base/featuregate"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "aws-iam-authenticator",
	Short: "A tool to authenticate to Kubernetes using AWS IAM credentials",
}

var featureGates = featuregate.NewFeatureGate()

func main() {
	Execute()
}

// Execute the CLI entrypoint
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "Load configuration from `filename`")

	rootCmd.PersistentFlags().StringP("log-format", "l", "text", "Specify log format to use when logging to stderr [text or json]")

	rootCmd.PersistentFlags().StringP("loglevel", "", "info", "Set the log level (debug, info, warn, error, fatal)")

	rootCmd.PersistentFlags().StringP(
		"cluster-id",
		"i",
		"",
		"Specify the cluster `ID`, a unique-per-cluster identifier for your aws-iam-authenticator installation.")
	viper.BindPFlag("clusterID", rootCmd.PersistentFlags().Lookup("cluster-id"))
	viper.BindEnv("clusterID", "KUBERNETES_AWS_AUTHENTICATOR_CLUSTER_ID")

	featureGates.Add(config.DefaultFeatureGates)
	featureGates.AddFlag(rootCmd.PersistentFlags())

	viper.BindPFlag("feature-gates", rootCmd.PersistentFlags().Lookup("feature-gates"))
}

func initConfig() {
	logrus.SetFormatter(getLogFormatter())
	logrus.SetLevel(getLogLevel())
	if cfgFile == "" {
		return
	}
	viper.SetConfigFile(cfgFile)
	if err := viper.ReadInConfig(); err != nil {
		fmt.Printf("Can't read configuration file %q: %v\n", cfgFile, err)
		os.Exit(1)
	}
}

func getConfig() (config.Config, error) {

	cfg := config.Config{
		PartitionID:                       viper.GetString("server.partition"),
		ClusterID:                         viper.GetString("clusterID"),
		ServerEC2DescribeInstancesRoleARN: viper.GetString("server.ec2DescribeInstancesRoleARN"),
		HostPort:                          viper.GetInt("server.port"),
		Hostname:                          viper.GetString("server.hostname"),
		GenerateKubeconfigPath:            viper.GetString("server.generateKubeconfig"),
		KubeconfigPregenerated:            viper.GetBool("server.kubeconfigPregenerated"),
		StateDir:                          viper.GetString("server.stateDir"),
		Address:                           viper.GetString("server.address"),
		Kubeconfig:                        viper.GetString("server.kubeconfig"),
		Master:                            viper.GetString("server.master"),
		BackendMode:                       viper.GetStringSlice("server.backendMode"),
		EC2DescribeInstancesQps:           viper.GetInt("server.ec2DescribeInstancesQps"),
		EC2DescribeInstancesBurst:         viper.GetInt("server.ec2DescribeInstancesBurst"),
		ScrubbedAWSAccounts:               viper.GetStringSlice("server.scrubbedAccounts"),
		//flags for dynamicfile mode
		//DynamicFilePath: the file path containing the roleMapping and userMapping
		DynamicFilePath: viper.GetString("server.dynamicfilepath"),
		//DynamicFileUserIDStrict: if true, then aws UserId from sts will be used to look up the roleMapping/userMapping; or aws IdentityArn is used
		DynamicFileUserIDStrict: viper.GetBool("server.dynamicfileUserIDStrict"),
		//DynamicBackendModePath: the file path containing the backend mode
		DynamicBackendModePath: viper.GetString("server.dynamicBackendModePath"),
	}
	if err := viper.UnmarshalKey("server.mapRoles", &cfg.RoleMappings); err != nil {
		return cfg, fmt.Errorf("invalid server role mappings: %v", err)
	}
	if err := viper.UnmarshalKey("server.mapUsers", &cfg.UserMappings); err != nil {
		logrus.WithError(err).Fatal("invalid server user mappings")
	}
	if err := viper.UnmarshalKey("server.mapAccounts", &cfg.AutoMappedAWSAccounts); err != nil {
		logrus.WithError(err).Fatal("invalid server account mappings")
	}
	var reservedPrefixConfig []config.ReservedPrefixConfig
	if err := viper.UnmarshalKey("server.reservedPrefixConfig", &reservedPrefixConfig); err != nil {
		return cfg, fmt.Errorf("invalid reserved prefix config: %v", err)
	}
	if len(reservedPrefixConfig) > 0 {
		cfg.ReservedPrefixConfig = make(map[string]config.ReservedPrefixConfig)
		for _, c := range reservedPrefixConfig {
			cfg.ReservedPrefixConfig[c.BackendMode] = c
		}
	}
	if featureGates.Enabled(config.SSORoleMatch) {
		logrus.Info("SSORoleMatch feature enabled")
		config.SSORoleMatchEnabled = true
	}
	if featureGates.Enabled(config.ConfiguredInitDirectories) {
		logrus.Info("ConfiguredInitDirectories feature enabled")
	}

	if cfg.ClusterID == "" {
		return cfg, errors.New("cluster ID cannot be empty")
	}

	partitionKeys := []string{}
	partitionMap := map[string]endpoints.Partition{}
	for _, p := range endpoints.DefaultPartitions() {
		partitionMap[p.ID()] = p
		partitionKeys = append(partitionKeys, p.ID())
	}
	if _, ok := partitionMap[cfg.PartitionID]; !ok {
		return cfg, errors.New("Invalid partition")
	}

	// DynamicFile BackendMode and DynamicFilePath are mutually inclusive.
	var dynamicFileModeSet bool
	for _, mode := range cfg.BackendMode {
		if mode == mapper.ModeDynamicFile {
			dynamicFileModeSet = true
		}
	}
	if dynamicFileModeSet && cfg.DynamicFilePath == "" {
		logrus.Fatal("dynamicfile is set in backend-mode but dynamicfilepath is not set")
	}
	if !dynamicFileModeSet && cfg.DynamicFilePath != "" {
		logrus.Fatal("dynamicfile is not set in backend-mode but dynamicfilepath is set")
	}

	if errs := mapper.ValidateBackendMode(cfg.BackendMode); len(errs) > 0 {
		return cfg, utilerrors.NewAggregate(errs)
	}

	return cfg, nil
}

func getLogFormatter() logrus.Formatter {
	format, _ := rootCmd.PersistentFlags().GetString("log-format")

	if format == "json" {
		return &logrus.JSONFormatter{}
	} else if format != "text" {
		logrus.Warnf("Unknown log format specified (%s), will use default text formatter instead.", format)
	}

	return &logrus.TextFormatter{FullTimestamp: true}
}

func getLogLevel() logrus.Level {
	logLevel, _ := rootCmd.PersistentFlags().GetString("loglevel")

	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logrus.Warnf("Unknown log format specified (%s), will use default text formatter instead.", logLevel)
		return logrus.InfoLevel
	}
	logrus.Info("Log level set to ", logLevel)
	return level
}
