package azure

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/pkg/errors"
	"github.com/rancher/norman/api/handler"
	"github.com/rancher/norman/httperror"
	"github.com/rancher/norman/types"
	v32 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/auth/providers/common"
	client "github.com/rancher/rancher/pkg/client/generated/management/v3"
	managementschema "github.com/rancher/rancher/pkg/schemas/management.cattle.io/v3"
	"github.com/sirupsen/logrus"
)

func (ap *azureProvider) formatter(apiContext *types.APIContext, resource *types.RawResource) {
	common.AddCommonActions(apiContext, resource)
	resource.AddAction(apiContext, "configureTest")
	resource.AddAction(apiContext, "testAndApply")
	resource.AddAction(apiContext, "upgrade")
}

func (ap *azureProvider) actionHandler(actionName string, action *types.Action, request *types.APIContext) error {
	handled, err := common.HandleCommonAction(actionName, action, request, Name, ap.authConfigs)
	if err != nil {
		return err
	}
	if handled {
		return nil
	}

	if actionName == "configureTest" {
		return ap.ConfigureTest(actionName, action, request)
	} else if actionName == "testAndApply" {
		return ap.testAndApply(actionName, action, request)
	} else if actionName == "upgrade" {
		return ap.migrateToMicrosoftGraph()
	}

	return httperror.NewAPIError(httperror.ActionNotAvailable, "")
}

func (ap *azureProvider) ConfigureTest(actionName string, action *types.Action, request *types.APIContext) error {
	// Verify the body has all required fields
	input, err := handler.ParseAndValidateActionBody(request, request.Schemas.Schema(&managementschema.Version,
		client.AzureADConfigType))
	if err != nil {
		return err
	}

	data := map[string]interface{}{
		"redirectUrl": formAzureRedirectURL(input),
		"type":        "azureADConfigTestOutput",
	}

	request.WriteResponse(http.StatusOK, data)
	return nil
}

func (ap *azureProvider) testAndApply(actionName string, action *types.Action, request *types.APIContext) error {
	azureADConfigApplyInput := &v32.AzureADConfigApplyInput{}
	if err := json.NewDecoder(request.Request.Body).Decode(azureADConfigApplyInput); err != nil {
		return httperror.NewAPIError(httperror.InvalidBodyContent,
			fmt.Sprintf("Failed to parse body: %v", err))
	}

	azureADConfig := &azureADConfigApplyInput.Config

	currentConfig, err := ap.getAzureConfigK8s()
	if err != nil {
		logrus.Errorf("Failed to fetch Azure AD Config from Kubernetes: %v", err)
		return httperror.NewAPIError(httperror.ServerError, "failed to fetch Azure AD Config from Kubernetes")
	}
	migrateNewFlowAnnotation(currentConfig, azureADConfig)

	azureLogin := &v32.AzureADLogin{
		Code: azureADConfigApplyInput.Code,
	}

	if azureADConfig.ApplicationSecret != "" {
		value, err := common.ReadFromSecret(ap.secrets, azureADConfig.ApplicationSecret,
			strings.ToLower(client.AzureADConfigFieldApplicationSecret))
		if err != nil {
			return err
		}
		azureADConfig.ApplicationSecret = value
	}
	//Call provider
	userPrincipal, groupPrincipals, providerToken, err := ap.loginUser(azureADConfig, azureLogin, true)
	if err != nil {
		if httperror.IsAPIError(err) {
			return err
		}
		return errors.Wrap(err, "server error while authenticating")
	}

	user, err := ap.userMGR.SetPrincipalOnCurrentUser(request, userPrincipal)
	if err != nil {
		return err
	}

	err = ap.saveAzureConfigK8s(azureADConfig)
	if err != nil {
		return httperror.NewAPIError(httperror.ServerError, fmt.Sprintf("Failed to save azure config: %v", err))
	}

	userExtraInfo := ap.GetUserExtraAttributes(userPrincipal)

	return ap.tokenMGR.CreateTokenAndSetCookie(user.Name, userPrincipal, groupPrincipals, providerToken, 0, "Token via Azure Configuration", request, userExtraInfo)
}

// Check the current auth config and make sure that the proposed one submitted through the API has up-to-date annotations.
// Rancher relies on GraphEndpointMigratedAnnotation to choose the right authentication flow and Graph API.
func migrateNewFlowAnnotation(current, proposed *v32.AzureADConfig) {
	if isConfigDeprecated(current) {
		return
	}
	// This covers the case where admins upgrade Rancher to v2.6.7+ without having used Azure AD as the auth provider.
	// In 2.6.7+, whether Azure AD is later registered or not, Rancher on startup creates the annotation on the template auth config.
	// But in the case where the auth config had been created on Rancher startup prior to v2.6.7, the annotation would be missing.
	// This ensures the annotation is set on initial attempt to set up Azure AD.
	// This also covers the case where admins want to reconfigure a v2.6.7+ new auth flow setup with a new secret or app.
	if proposed.ObjectMeta.Annotations == nil {
		proposed.ObjectMeta.Annotations = make(map[string]string)
	}
	proposed.ObjectMeta.Annotations[GraphEndpointMigratedAnnotation] = "true"
}
