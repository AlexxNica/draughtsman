package main

import (
	"net/http"
	"os"
	"time"

	"github.com/nlopes/slack"
	"github.com/spf13/afero"
	"github.com/spf13/viper"
	"k8s.io/client-go/kubernetes"

	"github.com/giantswarm/microkit/command"
	microserver "github.com/giantswarm/microkit/server"
	"github.com/giantswarm/microkit/transaction"
	"github.com/giantswarm/micrologger"
	"github.com/giantswarm/microstorage"
	"github.com/giantswarm/microstorage/memory"
	"github.com/giantswarm/operatorkit/client/k8s"

	"github.com/giantswarm/draughtsman/flag"
	"github.com/giantswarm/draughtsman/server"
	"github.com/giantswarm/draughtsman/service"
	"github.com/giantswarm/draughtsman/service/configurer/configmap"
	"github.com/giantswarm/draughtsman/service/configurer/secret"
	"github.com/giantswarm/draughtsman/service/deployer"
	"github.com/giantswarm/draughtsman/service/eventer/github"
	"github.com/giantswarm/draughtsman/service/installer/helm"
	slacknotifier "github.com/giantswarm/draughtsman/service/notifier/slack"
	slackspec "github.com/giantswarm/draughtsman/service/slack"
)

var (
	description string     = "draughtsman is an in-cluster agent that handles Helm based deployments."
	f           *flag.Flag = flag.New()
	gitCommit   string     = "n/a"
	name        string     = "draughtsman"
	source      string     = "https://github.com/giantswarm/draughtsman"
)

func main() {
	var err error

	var newLogger micrologger.Logger
	{
		loggerConfig := micrologger.DefaultConfig()
		loggerConfig.IOWriter = os.Stdout
		newLogger, err = micrologger.New(loggerConfig)
		if err != nil {
			panic(err)
		}
	}

	// We define a server factory to create the custom server once all command
	// line flags are parsed and all microservice configuration is sorted out.
	newServerFactory := func(v *viper.Viper) microserver.Server {
		var newHttpClient *http.Client
		{
			httpClientTimeout := v.GetDuration(f.Service.HTTPClient.Timeout)
			if httpClientTimeout.Seconds() == 0 {
				panic("http client timeout must be greater than zero")
			}

			newHttpClient = &http.Client{
				Timeout: httpClientTimeout,
			}
		}

		var newKubernetesClient kubernetes.Interface
		{
			k8sConfig := k8s.Config{
				Logger: newLogger,

				Address:   v.GetString(f.Service.Kubernetes.Address),
				InCluster: v.GetBool(f.Service.Kubernetes.InCluster),
				TLS: k8s.TLSClientConfig{
					CAFile:  v.GetString(f.Service.Kubernetes.TLS.CaFile),
					CrtFile: v.GetString(f.Service.Kubernetes.TLS.CrtFile),
					KeyFile: v.GetString(f.Service.Kubernetes.TLS.KeyFile),
				},
			}
			newKubernetesClient, err = k8s.NewClient(k8sConfig)
			if err != nil {
				panic(err)
			}
		}

		var newSlackClient slackspec.Client
		{
			newSlackClient = slack.New(v.GetString(f.Service.Slack.Token))
		}

		// Create a new custom service which implements business logic.
		var newService *service.Service
		{
			serviceConfig := service.DefaultConfig()

			serviceConfig.FileSystem = afero.NewOsFs()
			serviceConfig.HTTPClient = newHttpClient
			serviceConfig.KubernetesClient = newKubernetesClient
			serviceConfig.Logger = newLogger
			serviceConfig.SlackClient = newSlackClient

			serviceConfig.Flag = f
			serviceConfig.Viper = v

			serviceConfig.Description = description
			serviceConfig.GitCommit = gitCommit
			serviceConfig.Name = name
			serviceConfig.Source = source

			newService, err = service.New(serviceConfig)
			if err != nil {
				panic(err)
			}
		}

		var storage microstorage.Storage
		{
			storage, err = memory.New(memory.DefaultConfig())
			if err != nil {
				panic(err)
			}
		}

		var transactionResponder transaction.Responder
		{
			c := transaction.DefaultResponderConfig()
			c.Logger = newLogger
			c.Storage = storage

			transactionResponder, err = transaction.NewResponder(c)
			if err != nil {
				panic(err)
			}
		}

		// Create a new custom server which bundles our endpoints.
		var newServer microserver.Server
		{
			serverConfig := server.DefaultConfig()

			serverConfig.MicroServerConfig.Logger = newLogger
			serverConfig.MicroServerConfig.ServiceName = name
			serverConfig.MicroServerConfig.TransactionResponder = transactionResponder
			serverConfig.MicroServerConfig.Viper = v
			serverConfig.Service = newService

			newServer, err = server.New(serverConfig)
			if err != nil {
				panic(err)
			}
			go newService.Boot()
		}

		return newServer
	}

	// Create a new microkit command which manages our custom microservice.
	var newCommand command.Command
	{
		commandConfig := command.DefaultConfig()

		commandConfig.Logger = newLogger
		commandConfig.ServerFactory = newServerFactory

		commandConfig.Description = description
		commandConfig.GitCommit = gitCommit
		commandConfig.Name = name
		commandConfig.Source = source

		newCommand, err = command.New(commandConfig)
		if err != nil {
			panic(err)
		}
	}

	daemonCommand := newCommand.DaemonCommand().CobraCommand()

	daemonCommand.PersistentFlags().String(f.Service.Deployer.Environment, "", "Environment name that draughtsman is running in.")

	// Component type selection.
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Type, string(deployer.StandardDeployer), "Which deployer to use for deployment management.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Eventer.Type, string(github.GithubEventerType), "Which eventer to use for event management.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Type, string(helm.HelmInstallerType), "Which installer to use for installation management.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Configurer.Types, string(configmap.ConfigurerType)+","+string(secret.ConfigurerType), "Comma separated list of configurers to use for configuration management.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Notifier.Type, string(slacknotifier.SlackNotifierType), "Which notifier to use for notification management.")

	// Client configuration.
	daemonCommand.PersistentFlags().Duration(f.Service.HTTPClient.Timeout, 10*time.Second, "Timeout for HTTP requests.")

	daemonCommand.PersistentFlags().String(f.Service.Kubernetes.Address, "", "Address used to connect to Kubernetes. When empty in-cluster config is created.")
	daemonCommand.PersistentFlags().Bool(f.Service.Kubernetes.InCluster, true, "Whether to use the in-cluster config to authenticate with Kubernetes.")
	daemonCommand.PersistentFlags().String(f.Service.Kubernetes.TLS.CaFile, "", "Certificate authority file path to use to authenticate with Kubernetes.")
	daemonCommand.PersistentFlags().String(f.Service.Kubernetes.TLS.CrtFile, "", "Certificate file path to use to authenticate with Kubernetes.")
	daemonCommand.PersistentFlags().String(f.Service.Kubernetes.TLS.KeyFile, "", "Key file path to use to authenticate with Kubernetes.")

	daemonCommand.PersistentFlags().String(f.Service.Slack.Token, "", "Token to post Slack notifications with.")

	// Service configuration.
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Eventer.GitHub.OAuthToken, "", "OAuth token for authenticating against GitHub. Needs 'repo_deployment' scope.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Eventer.GitHub.Organisation, "", "Organisation under which to check for deployments.")
	daemonCommand.PersistentFlags().Duration(f.Service.Deployer.Eventer.GitHub.PollInterval, 1*time.Minute, "Interval to poll for new deployments.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Eventer.GitHub.ProjectList, "", "Comma seperated list of GitHub projects to check for deployments.")

	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Helm.HelmBinaryPath, "/bin/helm", "Path to Helm binary. Needs CNR registry plugin installed.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Helm.Organisation, "", "Organisation of Helm CNR registry.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Helm.Password, "", "Password for Helm CNR registry.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Helm.Registry, "quay.io", "URL for Helm CNR registry.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Helm.Username, "", "Username for Helm CNR registry.")

	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Configurer.ConfigMap.Key, "values", "Key in configmap holding values data.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Configurer.ConfigMap.Name, "draughtsman-values-configmap", "Name of configmap holding values data.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Configurer.ConfigMap.Namespace, "draughtsman", "Namespace of configmap holding values data.")

	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Configurer.File.Path, "", "Path to values file.")

	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Configurer.Secret.Key, "values", "Key in secret holding values data.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Configurer.Secret.Name, "draughtsman-values-secret", "Name of secret holding values data.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Installer.Configurer.Secret.Namespace, "draughtsman", "Namespace of secret holding values data.")

	daemonCommand.PersistentFlags().String(f.Service.Deployer.Notifier.Slack.Channel, "", "Channel to post Slack notifications to.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Notifier.Slack.Emoji, ":older_man:", "Emoji to use for Slack notifications.")
	daemonCommand.PersistentFlags().String(f.Service.Deployer.Notifier.Slack.Username, "draughtsman", "Username to post Slack notifications with.")

	newCommand.CobraCommand().Execute()
}
