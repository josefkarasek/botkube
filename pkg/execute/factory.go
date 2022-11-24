package execute

import (
	"context"

	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"

	"github.com/kubeshop/botkube/internal/plugin"
	"github.com/kubeshop/botkube/pkg/bot/interactive"
	"github.com/kubeshop/botkube/pkg/config"
	"github.com/kubeshop/botkube/pkg/execute/command"
	"github.com/kubeshop/botkube/pkg/execute/kubectl"
	"github.com/kubeshop/botkube/pkg/filterengine"
)

// DefaultExecutorFactory facilitates creation of the Executor instances.
type DefaultExecutorFactory struct {
	log               logrus.FieldLogger
	cfg               config.Config
	filterEngine      filterengine.FilterEngine
	analyticsReporter AnalyticsReporter
	notifierExecutor  *NotifierExecutor
	kubectlExecutor   *Kubectl
	pluginExecutor    *PluginExecutor
	editExecutor      *EditExecutor
	actionExecutor    *ActionExecutor
	commandExecutor   *CommandsExecutor
	filterExecutor    *FilterExecutor
	pingExecutor      *PingExecutor
	versionExecutor   *VersionExecutor
	helpExecutor      *HelpExecutor
	feedbackExecutor  *FeedbackExecutor
	merger            *kubectl.Merger
	cfgManager        ConfigPersistenceManager
	kubectlCmdBuilder *KubectlCmdBuilder
	cmdsMapping       map[CommandVerb]map[string]CommandFn
}

// DefaultExecutorFactoryParams contains input parameters for DefaultExecutorFactory.
type DefaultExecutorFactoryParams struct {
	Log               logrus.FieldLogger
	CmdRunner         CommandRunner
	Cfg               config.Config
	FilterEngine      filterengine.FilterEngine
	KcChecker         *kubectl.Checker
	Merger            *kubectl.Merger
	CfgManager        ConfigPersistenceManager
	AnalyticsReporter AnalyticsReporter
	NamespaceLister   NamespaceLister
	CommandGuard      CommandGuard
	PluginManager     *plugin.Manager
	BotKubeVersion    string
}

// Executor is an interface for processes to execute commands
type Executor interface {
	Execute(context.Context) interactive.Message
}

// ConfigPersistenceManager manages persistence of the configuration.
type ConfigPersistenceManager interface {
	PersistSourceBindings(ctx context.Context, commGroupName string, platform config.CommPlatformIntegration, channelAlias string, sourceBindings []string) error
	PersistNotificationsEnabled(ctx context.Context, commGroupName string, platform config.CommPlatformIntegration, channelAlias string, enabled bool) error
	PersistFilterEnabled(ctx context.Context, name string, enabled bool) error
	PersistActionEnabled(ctx context.Context, name string, enabled bool) error
}

// AnalyticsReporter defines a reporter that collects analytics data.
type AnalyticsReporter interface {
	// ReportCommand reports a new executed command. The command should be anonymized before using this method.
	ReportCommand(platform config.CommPlatformIntegration, command string, origin command.Origin, withFilter bool) error
}

// CommandGuard is an interface that allows to check if a given command is allowed to be executed.
type CommandGuard interface {
	GetAllowedResourcesForVerb(verb string, allConfiguredResources []string) ([]kubectl.Resource, error)
	GetResourceDetails(verb, resourceType string) (kubectl.Resource, error)
	FilterSupportedVerbs(allVerbs []string) []string
}

// NewExecutorFactory creates new DefaultExecutorFactory.
func NewExecutorFactory(params DefaultExecutorFactoryParams) *DefaultExecutorFactory {
	kcExecutor := NewKubectl(
		params.Log.WithField("component", "Kubectl Executor"),
		params.Cfg,
		params.Merger,
		params.KcChecker,
		params.CmdRunner,
	)
	actionExecutor := NewActionExecutor(
		params.Log.WithField("component", "Botkube Action Executor"),
		params.AnalyticsReporter,
		params.CfgManager,
		params.Cfg,
	)
	editExecutor := NewEditExecutor(
		params.Log.WithField("component", "Botkube Edit Executor"),
		params.AnalyticsReporter,
		params.CfgManager,
		params.Cfg,
	)
	commandExecutor := NewCommandsExecutor(
		params.Log.WithField("component", "Botkube Command Executor"),
		params.AnalyticsReporter,
		params.Merger,
	)
	filterExecutor := NewFilterExecutor(
		params.Log.WithField("component", "Botkube Filter Executor"),
		params.AnalyticsReporter,
		params.CfgManager,
		params.FilterEngine,
	)
	pingExecutor := NewPingExecutor(
		params.Log.WithField("component", "Botkube Ping Executor"),
		params.AnalyticsReporter,
		params.BotKubeVersion,
	)
	versionExecutor := NewVersionExecutor(
		params.Log.WithField("component", "Botkube Version Executor"),
		params.AnalyticsReporter,
		params.BotKubeVersion,
	)
	feedbackExecutor := NewFeedbackExecutor(
		params.Log.WithField("component", "Botkube Feedback Executor"),
		params.AnalyticsReporter,
	)
	notifierExecutor := NewNotifierExecutor(
		params.Log.WithField("component", "Notifier Executor"),
		params.Cfg,
		params.CfgManager,
		params.AnalyticsReporter,
	)
	helpExecutor := NewHelpExecutor(
		params.Log.WithField("component", "Botkube Help Executor"),
		params.AnalyticsReporter,
	)

	executors := []CommandExecutor{
		actionExecutor,
		editExecutor,
		commandExecutor,
		filterExecutor,
		pingExecutor,
		versionExecutor,
		helpExecutor,
		feedbackExecutor,
		notifierExecutor,
	}
	return &DefaultExecutorFactory{
		log:               params.Log,
		cfg:               params.Cfg,
		filterEngine:      params.FilterEngine,
		analyticsReporter: params.AnalyticsReporter,
		notifierExecutor:  notifierExecutor,
		kubectlCmdBuilder: NewKubectlCmdBuilder(
			params.Log.WithField("component", "Kubectl Command Builder"),
			params.Merger,
			kcExecutor,
			params.NamespaceLister,
			params.CommandGuard,
		),
		pluginExecutor: NewPluginExecutor(
			params.Log.WithField("component", "Botkube Plugin Executor"),
			params.Cfg,
			params.PluginManager,
		),
		editExecutor:     editExecutor,
		actionExecutor:   actionExecutor,
		commandExecutor:  commandExecutor,
		filterExecutor:   filterExecutor,
		pingExecutor:     pingExecutor,
		versionExecutor:  versionExecutor,
		helpExecutor:     helpExecutor,
		feedbackExecutor: feedbackExecutor,
		merger:           params.Merger,
		cfgManager:       params.CfgManager,
		kubectlExecutor:  kcExecutor,
		cmdsMapping:      newCmdsMapping(executors),
	}
}

// Conversation contains details about the conversation.
type Conversation struct {
	Alias            string
	ID               string
	ExecutorBindings []string
	IsAuthenticated  bool
	CommandOrigin    command.Origin
	State            *slack.BlockActionStates
}

// NewDefaultInput an input for NewDefault
type NewDefaultInput struct {
	CommGroupName   string
	Platform        config.CommPlatformIntegration
	NotifierHandler NotifierHandler
	Conversation    Conversation
	Message         string
	User            string
}

// NewDefault creates new Default Executor.
func (f *DefaultExecutorFactory) NewDefault(cfg NewDefaultInput) Executor {
	return &DefaultExecutor{
		log:               f.log,
		cfg:               f.cfg,
		analyticsReporter: f.analyticsReporter,
		kubectlExecutor:   f.kubectlExecutor,
		pluginExecutor:    f.pluginExecutor,
		notifierExecutor:  f.notifierExecutor,
		editExecutor:      f.editExecutor,
		actionExecutor:    f.actionExecutor,
		commandExecutor:   f.commandExecutor,
		filterExecutor:    f.filterExecutor,
		filterEngine:      f.filterEngine,
		pingExecutor:      f.pingExecutor,
		versionExecutor:   f.versionExecutor,
		helpExecutor:      f.helpExecutor,
		feedbackExecutor:  f.feedbackExecutor,
		merger:            f.merger,
		cfgManager:        f.cfgManager,
		kubectlCmdBuilder: f.kubectlCmdBuilder,
		cmdsMapping:       f.cmdsMapping,
		user:              cfg.User,
		notifierHandler:   cfg.NotifierHandler,
		conversation:      cfg.Conversation,
		message:           cfg.Message,
		platform:          cfg.Platform,
		commGroupName:     cfg.CommGroupName,
	}
}
