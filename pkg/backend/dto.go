package backend

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/k8sgpt-ai/k8sgpt-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Info struct {
	BaseUrl      string
	AccessSecret *v1alpha1.SecretRef
}

const (
	accessSecretTokenKey   = "access-token"
	accessSecretName       = "ai-cred"
	accessSecretBaseUrl    = "base_url"
	accessSecretLabelKey   = "operator.kyma-project.io/managed-by"
	accessSecretLabelValue = "ai-sre"
)

var (
	ErrNoSecretConfigured  = errors.New("references secret not configured")
	ErrSecretNotFound      = errors.New("references secret does not exist, cannot create deployment")
	ErrSecretMalformed     = errors.New("references secret data incorrect")
	ErrNoBackendUrl        = errors.New("references secret not contains backend url")
	ErrWaitForAccessSecret = errors.New("access secret not created yet")
	ErrRequestReject       = errors.New("request reject")
)

func GetBackendInfo(ctx context.Context, k8sClient client.Client, config *v1alpha1.K8sGPT, tokenExpireTime time.Duration) (*Info, error) {
	backendInfo, err := getBackendInfoFromSecret(ctx, k8sClient, tokenExpireTime)
	if err == nil {
		return backendInfo, nil
	}
	if !errors.Is(ErrWaitForAccessSecret, err) {
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
	if err := saveToSecret(ctx, k8sClient, config.Namespace, string(baseUrl), accessToken.AccessToken); err != nil {
		return nil, err
	}

	return nil, ErrWaitForAccessSecret
}

func fetchAccessToken(ctx context.Context, oauthUrl, clientId, clientSecret string) (*AccessToken, error) {
	data := url.Values{}
	data.Set("client_id", clientId)
	data.Set("grant_type", "client_credentials")

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthUrl+"/oauth/token", strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	basicAuth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", clientId, clientSecret)))
	request.Header.Add("Authorization", fmt.Sprintf("Basic %s", basicAuth))
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	clnt := &http.Client{}
	response, err := clnt.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("%w: actual status code: %d", ErrRequestReject, response.StatusCode)
	}
	defer response.Body.Close()
	bodyBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	token := &AccessToken{}
	err = yaml.Unmarshal(bodyBytes, token)
	if err != nil {
		return nil, err
	}
	return token, nil
}

func saveToSecret(ctx context.Context, k8sClient client.Client, namespace, baseUrl, accessToken string) error {
	secret := &corev1.Secret{}
	secret.SetName(generateSecretName(accessToken))
	secret.SetNamespace(namespace)
	if secret.StringData == nil {
		secret.StringData = map[string]string{}
	}
	if secret.Labels == nil {
		secret.Labels = map[string]string{}
	}
	secret.Labels[accessSecretLabelKey] = accessSecretLabelValue
	secret.StringData[accessSecretBaseUrl] = baseUrl
	secret.StringData[accessSecretTokenKey] = accessToken
	if err := k8sClient.Create(ctx, secret); err != nil {
		return errors.New("can't create access secret")
	}
	return nil
}

func generateSecretName(token string) string {
	h := sha1.New()
	h.Write([]byte(token))
	return fmt.Sprintf("%s-%s", accessSecretName, hex.EncodeToString(h.Sum(nil)))
}

func getBackendInfoFromSecret(ctx context.Context, k8sClient client.Client, tokenExpireTime time.Duration) (*Info, error) {

	secretList := corev1.SecretList{}
	err := k8sClient.List(
		ctx, &secretList, &client.ListOptions{
			LabelSelector: labels.SelectorFromSet(labels.Set{accessSecretLabelKey: accessSecretLabelValue}),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list cred secrets: %w", err)
	}

	if len(secretList.Items) == 0 {
		return nil, ErrWaitForAccessSecret
	}

	notSelected := true
	latestSecret := &corev1.Secret{}

	for _, secret := range secretList.Items {
		secret := secret
		if notSelected {
			notSelected = false
			latestSecret = &secret
		} else if latestSecret.CreationTimestamp.Before(&secret.CreationTimestamp) {
			latestSecret = &secret
		}
	}

	deleteOldSecret(ctx, k8sClient, secretList, latestSecret)

	if latestSecret.CreationTimestamp.Add(tokenExpireTime).Before(time.Now()) {
		_ = k8sClient.Delete(ctx, latestSecret)
	}

	return &Info{
		AccessSecret: &v1alpha1.SecretRef{
			Name: latestSecret.Name,
			Key:  accessSecretTokenKey,
		},
		BaseUrl: string(latestSecret.Data[accessSecretBaseUrl]),
	}, nil
}

func deleteOldSecret(ctx context.Context, k8sClient client.Client, secretList corev1.SecretList, latestSecret *corev1.Secret) {
	for _, secret := range secretList.Items {
		secret := secret
		if secret.Name != latestSecret.Name {
			_ = k8sClient.Delete(ctx, &secret)
		}
	}
}
