package backend

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/k8sgpt-ai/k8sgpt-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type BackendInfo struct {
	BaseUrl      string
	AccessSecret *v1alpha1.SecretRef
}

const ACCESS_SECRET_NAME = "ai-cred"
const ACCESS_SECRET_TOKEN = "access-token"
const ACCESS_SECRET_BASE_URL = "base_url"

var (
	ErrNoSecretConfigured  = errors.New("references secret not configured")
	ErrSecretNotFound      = errors.New("references secret does not exist, cannot create deployment")
	ErrSecretMalformed     = errors.New("references secret data incorrect")
	ErrNoBackendUrl        = errors.New("references secret not contains backend url")
	ErrWaitForAccessSecret = errors.New("access secret not created yet")
	ErrRequestReject       = errors.New("request reject")
)

func GetBackendInfo(ctx context.Context, k8sClient client.Client, config *v1alpha1.K8sGPT) (*BackendInfo, error) {
	backendInfo, err := getBackendInfoFromSecret(ctx, k8sClient, config.Namespace)
	if err == nil {
		return backendInfo, nil
	}
	if !k8sErrors.IsNotFound(err) {
		return nil, err
	}

	if config.Spec.AI.Secret == nil {
		return nil, ErrNoSecretConfigured
	}
	//get service binding secret
	secret := &corev1.Secret{}

	if err := k8sClient.Get(ctx, types.NamespacedName{Name: config.Spec.AI.Secret.Name,
		Namespace: config.Namespace}, secret); err != nil {
		return nil, ErrSecretNotFound
	}
	uaa := &UAA{}
	err = yaml.Unmarshal(secret.Data["uaa"], uaa)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSecretMalformed, err)
	}
	accessToken, err := fetchAccessToken(ctx, uaa.OAuthUrl, uaa.ClientId, uaa.ClientSecret)
	if err != nil {
		return nil, err
	}
	baseUrl, found := secret.Data["url"]
	if !found {
		return nil, ErrNoBackendUrl
	}
	if err := saveToSecret(ctx, k8sClient, config.Namespace, string(baseUrl), accessToken); err != nil {
		return nil, err
	}

	return nil, ErrWaitForAccessSecret
}

func fetchAccessToken(ctx context.Context, oauthUrl, clientId, clientSecret string) (string, error) {
	data := url.Values{}
	data.Set("client_id", clientId)
	data.Set("grant_type", "client_credentials")

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthUrl+"/oauth/token", strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	basicAuth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", clientId, clientSecret)))
	request.Header.Add("Authorization", fmt.Sprintf("Basic %s", basicAuth))
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	clnt := &http.Client{}
	response, err := clnt.Do(request)
	if err != nil {
		return "", err
	}
	if response.StatusCode != 200 {
		return "", fmt.Errorf("%w: actual status code: %d", ErrRequestReject, response.StatusCode)
	}
	defer response.Body.Close()
	bodyBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	token := &Token{}
	err = yaml.Unmarshal(bodyBytes, token)
	if err != nil {
		return "", err
	}
	return token.AccessToken, nil
}

func saveToSecret(ctx context.Context, k8sClient client.Client, namespace, baseUrl, accessToken string) error {
	secret := &corev1.Secret{}
	secret.SetName(ACCESS_SECRET_NAME)
	secret.SetNamespace(namespace)
	if secret.StringData == nil {
		secret.StringData = map[string]string{}
	}

	secret.StringData[ACCESS_SECRET_BASE_URL] = baseUrl
	secret.StringData[ACCESS_SECRET_TOKEN] = accessToken
	if err := k8sClient.Create(ctx, secret); err != nil {
		return errors.New("can't create access secret")
	}
	return nil
}

func getBackendInfoFromSecret(ctx context.Context, k8sClient client.Client, namespace string) (*BackendInfo, error) {
	secret := &corev1.Secret{}

	if err := k8sClient.Get(ctx, types.NamespacedName{Name: ACCESS_SECRET_NAME,
		Namespace: namespace}, secret); err != nil {
		return nil, err
	}

	return &BackendInfo{
		AccessSecret: &v1alpha1.SecretRef{
			Name: ACCESS_SECRET_NAME,
			Key:  ACCESS_SECRET_TOKEN,
		}, BaseUrl: string(secret.Data[ACCESS_SECRET_BASE_URL])}, nil
}
