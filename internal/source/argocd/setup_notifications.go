package argocd

import (
	"context"
	"fmt"
	"regexp"

	"github.com/MakeNowJust/heredoc"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/kubeshop/botkube/internal/source/kubernetes/k8sutil"
	"github.com/kubeshop/botkube/pkg/multierror"
)

var (
	configMapGVR = schema.GroupVersionResource{
		Version:  "v1",
		Resource: "configmaps",
	}
	argoAppGVR = schema.GroupVersionResource{
		Group:    "argoproj.io",
		Version:  "v1alpha1",
		Resource: "applications",
	}
	allowedCharsRegex = regexp.MustCompile(`[^a-zA-Z0-9]+`)
)

const (
	namePrefix = "b"

	fieldManagerName = "botkube"

	appAnnotationPatchFmt = `{"metadata":{"annotations":{"%s":""}}}`

	annotationKeyFmt = "notifications.argoproj.io/subscribe.%s.%s"

	// the K8s annotation needs to be 63 chars or fewer.
	// `notifications.argoproj.io/subscribe..` is already 37 chars, so we have 26 chars to spend
	maxWebhookNameLength  = 6
	maxTriggerNameLength  = 20
	maxTemplateNameLength = 128 // there's no actual limit apart from 1MB for the ConfigMap, but let's be reasonable
)

func (s *Source) setupArgoNotifications(ctx context.Context, k8sCli *dynamic.DynamicClient) error {
	cm, err := s.getConfigMap(ctx, k8sCli)
	if err != nil {
		return fmt.Errorf("while getting ArgoCD config map: %w", err)
	}

	webhookName, err := s.renderWebhookName(s.cfg.Webhook.Name, s.srcCtx)
	if err != nil {
		return err
	}
	s.log.Debugf("Using webhook %q...", webhookName)

	// register webhook
	if s.cfg.Webhook.Register {
		path, value, err := s.registerWebhook(webhookName)
		if err != nil {
			return fmt.Errorf("while registering webhook %q: %w", webhookName, err)
		}

		cm.Data[path] = value
	}

	// register templates
	errs := multierror.New()
	s.log.Info("Registering templates...")
	for _, tpl := range s.cfg.Templates {
		path, value, err := s.registerTemplate(webhookName, tpl)
		if err != nil {
			errs = multierror.Append(errs, fmt.Errorf("while registering template %q: %w", tpl.Name, err))
		}

		cm.Data[path] = value
	}

	var subs []subscription
	s.log.Debug("Registering triggers...")
	for _, notification := range s.cfg.Notifications {
		// register triggers
		if notification.Trigger.FromExisting == nil && notification.Trigger.Create == nil {
			errs = multierror.Append(errs, fmt.Errorf("either trigger.fromExisting or trigger.create must be set"))
			continue
		}

		var (
			triggerName    string
			triggerDetails []TriggerCondition
		)
		if notification.Trigger.FromExisting != nil {
			triggerName, triggerDetails, err = s.useExistingTrigger(cm, *notification.Trigger.FromExisting)
			if err != nil {
				errs = multierror.Append(errs, fmt.Errorf("while using existing trigger: %w", err))
				continue
			}
		}

		if notification.Trigger.Create != nil {
			triggerName, triggerDetails, err = s.createTrigger(*notification.Trigger.Create)
			if err != nil {
				errs = multierror.Append(errs, fmt.Errorf("while creating new trigger: %w", err))
				continue
			}
		}

		triggerPath := fmt.Sprintf("trigger.%s", triggerName)
		bytes, err := yaml.Marshal(triggerDetails)
		if err != nil {
			errs = multierror.Append(errs, fmt.Errorf("while marshalling trigger details for %q: %w", triggerPath, err))
			continue
		}
		cm.Data[triggerPath] = string(bytes)

		apps := s.cfg.DefaultSubscriptions.Applications
		if notification.Subscriptions.Create {
			apps = append(apps, notification.Subscriptions.Applications...)
		}
		for _, app := range apps {
			subs = append(subs, subscription{
				TriggerName: triggerName,
				WebhookName: webhookName,
				Application: app,
			})
		}
	}
	if errs.ErrorOrNil() != nil {
		return fmt.Errorf("while configuring Argo notifications: %w", errs.ErrorOrNil())
	}

	err = s.updateConfigMap(ctx, k8sCli, cm)
	if err != nil {
		return fmt.Errorf("while updating ArgoCD config map: %w", err)
	}

	// annotate Applications
	err = s.createSubscriptions(ctx, k8sCli, subs)
	if err != nil {
		return fmt.Errorf("while creating subscriptions: %w", err)
	}

	return nil
}

func (s *Source) getConfigMap(ctx context.Context, k8sCli *dynamic.DynamicClient) (v1.ConfigMap, error) {
	notifCfgMap := s.cfg.ArgoCD.NotificationsConfigMap
	unstrCM, err := k8sCli.Resource(configMapGVR).Namespace(notifCfgMap.Namespace).Get(ctx, notifCfgMap.Name, metav1.GetOptions{})
	if err != nil {
		return v1.ConfigMap{}, fmt.Errorf("while getting ArgoCD config map: %w", err)
	}

	var cm v1.ConfigMap
	err = k8sutil.TransformIntoTypedObject(unstrCM, &cm)
	if err != nil {
		return v1.ConfigMap{}, fmt.Errorf("while transforming object type %T into type: %T: %w", unstrCM, cm, err)
	}

	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}

	return cm, nil
}

func (s *Source) updateConfigMap(ctx context.Context, k8sCli *dynamic.DynamicClient, cm v1.ConfigMap) error {
	s.log.Debug("Updating ConfigMap...")

	unstrCM, err := k8sutil.TransformIntoUnstructured(&cm)
	if err != nil {
		return fmt.Errorf("while transforming object type %T into type: %T: %w", cm, unstrCM, err)
	}

	_, err = k8sCli.Resource(configMapGVR).Namespace(cm.Namespace).Update(ctx, unstrCM, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("while updating ArgoCD config map: %w", err)
	}

	return nil
}

func (s *Source) useExistingTrigger(cm v1.ConfigMap, triggerCfg TriggerFromExisting) (string, []TriggerCondition, error) {
	existingTriggerName, err := renderStringIfTemplate(triggerCfg.Name, s.srcCtx)
	if err != nil {
		return "", nil, fmt.Errorf("while rendering trigger name: %w", err)
	}
	originalTriggerPath := fmt.Sprintf("trigger.%s", existingTriggerName)
	if cm.Data[originalTriggerPath] == "" {
		return "", nil, fmt.Errorf("trigger %q does not exist", originalTriggerPath)
	}

	clonedTriggerName := fmt.Sprintf("%s-%s-%s", namePrefix, s.srcCtx.SourceName, existingTriggerName)
	triggerName, err := s.renderTriggerName(clonedTriggerName, s.srcCtx)
	if err != nil {
		return "", nil, err
	}

	s.log.WithFields(logrus.Fields{
		"originalTriggerPath": originalTriggerPath,
		"triggerName":         triggerName,
	}).Debug("Reusing trigger...")

	var triggerDetails []TriggerCondition
	err = yaml.Unmarshal([]byte(cm.Data[originalTriggerPath]), &triggerDetails)
	if err != nil {
		return "", nil, fmt.Errorf("while unmarshalling trigger details for %q: %w", originalTriggerPath, err)
	}

	templateName, err := s.renderTemplateName(triggerCfg.TemplateName, s.srcCtx)
	if err != nil {
		return "", nil, err
	}

	s.log.Debug("Modifying new trigger...")
	for i := range triggerDetails {
		triggerDetails[i].Send = []string{templateName}
	}

	return triggerName, triggerDetails, nil
}

func (s *Source) createTrigger(triggerCfg NewTrigger) (string, []TriggerCondition, error) {
	triggerName, err := s.renderTriggerName(triggerCfg.Name, s.srcCtx)
	if err != nil {
		return "", nil, err
	}

	s.log.Debugf("Creating new trigger %q...", triggerName)

	errs := multierror.New()
	triggerDetails := triggerCfg.Conditions
	for i, details := range triggerDetails {
		for j, sendDetails := range details.Send {
			templateName, err := s.renderTemplateName(sendDetails, s.srcCtx)
			if err != nil {
				errs = multierror.Append(errs, err)
				continue
			}

			triggerDetails[i].Send[j] = templateName
		}
	}

	return triggerName, triggerDetails, nil
}

func (s *Source) createSubscriptions(ctx context.Context, k8sCli *dynamic.DynamicClient, subs []subscription) error {
	s.log.Info("Annotating applications...")
	errs := multierror.New()
	for _, sub := range subs {
		if sub.Application.Name == "" || sub.Application.Namespace == "" {
			errs = multierror.Append(errs, fmt.Errorf("application name and namespace must be set"))
		}

		annotationKey := fmt.Sprintf(annotationKeyFmt, sub.TriggerName, sub.WebhookName)
		s.log.WithField("annotationKey", annotationKey).Debugf("Annotating application \"%s/%s\"...", sub.Application.Namespace, sub.Application.Name)
		annotationPatch := fmt.Sprintf(appAnnotationPatchFmt, annotationKey)
		_, err := k8sCli.Resource(argoAppGVR).Namespace(sub.Application.Namespace).Patch(
			ctx,
			sub.Application.Name,
			types.MergePatchType,
			[]byte(annotationPatch),
			metav1.PatchOptions{FieldManager: fieldManagerName})
		if err != nil {
			errs = multierror.Append(errs, fmt.Errorf("while annotating application \"%s/%s\": %w", sub.Application.Namespace, sub.Application.Name, err))
			continue
		}
	}
	if errs.ErrorOrNil() != nil {
		return fmt.Errorf("while annotating Argo applications: %w", errs.ErrorOrNil())
	}

	return nil
}

func (s *Source) registerWebhook(webhookName string) (string, string, error) {
	s.log.Info("Registering webhook...")

	webhookURL, err := renderStringIfTemplate(s.cfg.Webhook.URL, s.srcCtx)
	if err != nil {
		return "", "", fmt.Errorf("while rendering webhook URL: %w", err)
	}

	path := fmt.Sprintf("service.webhook.%s", webhookName)
	value := heredoc.Docf(`
			url: %s
		`, webhookURL)

	return path, value, nil
}

type webhookConfig struct {
	Method string `json:"method"`
	Body   string `json:"body"`
}

func (s *Source) registerTemplate(webhookName string, tpl Template) (string, string, error) {
	templateName, err := s.renderTemplateName(tpl.Name, s.srcCtx)
	if err != nil {
		return "", "", err
	}

	s.log.Debugf("Registering template %q...", templateName)

	out := map[string]interface{}{
		"webhook": map[string]interface{}{
			webhookName: webhookConfig{
				Method: "POST",
				Body:   tpl.Body,
			},
		},
	}

	bytes, err := yaml.Marshal(out)
	if err != nil {
		return "", "", fmt.Errorf("while marshalling template %q: %w", templateName, err)
	}

	tplPath := fmt.Sprintf("template.%s", templateName)
	tplValue := string(bytes)

	return tplPath, tplValue, nil
}
