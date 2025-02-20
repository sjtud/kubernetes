// +build integration,!no-etcd

/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

package integration

// This file tests authentication and (soon) authorization of HTTP requests to a master object.
// It does not use the client in pkg/client/... because authentication and authorization needs
// to work for any client of the HTTP interface.

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/testapi"
	"k8s.io/kubernetes/pkg/auth/authenticator"
	"k8s.io/kubernetes/pkg/auth/authenticator/bearertoken"
	"k8s.io/kubernetes/pkg/auth/authorizer"
	"k8s.io/kubernetes/pkg/auth/user"
	clientset "k8s.io/kubernetes/pkg/client/clientset_generated/release_1_2"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	serviceaccountcontroller "k8s.io/kubernetes/pkg/controller/serviceaccount"
	"k8s.io/kubernetes/pkg/master"
	"k8s.io/kubernetes/pkg/serviceaccount"
	"k8s.io/kubernetes/pkg/util/sets"
	"k8s.io/kubernetes/pkg/util/wait"
	serviceaccountadmission "k8s.io/kubernetes/plugin/pkg/admission/serviceaccount"
	"k8s.io/kubernetes/plugin/pkg/auth/authenticator/request/union"
	"k8s.io/kubernetes/test/integration/framework"
)

const (
	rootUserName = "root"
	rootToken    = "root-user-token"

	readOnlyServiceAccountName  = "ro"
	readWriteServiceAccountName = "rw"
)

func init() {
	requireEtcd()
}

func TestServiceAccountAutoCreate(t *testing.T) {
	c, _, stopFunc := startServiceAccountTestServer(t)
	defer stopFunc()

	ns := "test-service-account-creation"

	// Create namespace
	_, err := c.Legacy().Namespaces().Create(&api.Namespace{ObjectMeta: api.ObjectMeta{Name: ns}})
	if err != nil {
		t.Fatalf("could not create namespace: %v", err)
	}

	// Get service account
	defaultUser, err := getServiceAccount(c, ns, "default", true)
	if err != nil {
		t.Fatalf("Default serviceaccount not created: %v", err)
	}

	// Delete service account
	err = c.Legacy().ServiceAccounts(ns).Delete(defaultUser.Name, nil)
	if err != nil {
		t.Fatalf("Could not delete default serviceaccount: %v", err)
	}

	// Get recreated service account
	defaultUser2, err := getServiceAccount(c, ns, "default", true)
	if err != nil {
		t.Fatalf("Default serviceaccount not created: %v", err)
	}
	if defaultUser2.UID == defaultUser.UID {
		t.Fatalf("Expected different UID with recreated serviceaccount")
	}
}

func TestServiceAccountTokenAutoCreate(t *testing.T) {
	c, _, stopFunc := startServiceAccountTestServer(t)
	defer stopFunc()

	ns := "test-service-account-token-creation"
	name := "my-service-account"

	// Create namespace
	_, err := c.Legacy().Namespaces().Create(&api.Namespace{ObjectMeta: api.ObjectMeta{Name: ns}})
	if err != nil {
		t.Fatalf("could not create namespace: %v", err)
	}

	// Create service account
	serviceAccount, err := c.Legacy().ServiceAccounts(ns).Create(&api.ServiceAccount{ObjectMeta: api.ObjectMeta{Name: name}})
	if err != nil {
		t.Fatalf("Service Account not created: %v", err)
	}

	// Get token
	token1Name, token1, err := getReferencedServiceAccountToken(c, ns, name, true)
	if err != nil {
		t.Fatal(err)
	}

	// Delete token
	err = c.Legacy().Secrets(ns).Delete(token1Name, nil)
	if err != nil {
		t.Fatalf("Could not delete token: %v", err)
	}

	// Get recreated token
	token2Name, token2, err := getReferencedServiceAccountToken(c, ns, name, true)
	if err != nil {
		t.Fatal(err)
	}
	if token1Name == token2Name {
		t.Fatalf("Expected new auto-created token name")
	}
	if token1 == token2 {
		t.Fatalf("Expected new auto-created token value")
	}

	// Trigger creation of a new referenced token
	serviceAccount, err = c.Legacy().ServiceAccounts(ns).Get(name)
	if err != nil {
		t.Fatal(err)
	}
	serviceAccount.Secrets = []api.ObjectReference{}
	_, err = c.Legacy().ServiceAccounts(ns).Update(serviceAccount)
	if err != nil {
		t.Fatal(err)
	}

	// Get rotated token
	token3Name, token3, err := getReferencedServiceAccountToken(c, ns, name, true)
	if err != nil {
		t.Fatal(err)
	}
	if token3Name == token2Name {
		t.Fatalf("Expected new auto-created token name")
	}
	if token3 == token2 {
		t.Fatalf("Expected new auto-created token value")
	}

	// Delete service account
	err = c.Legacy().ServiceAccounts(ns).Delete(name, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for tokens to be deleted
	tokensToCleanup := sets.NewString(token1Name, token2Name, token3Name)
	err = wait.Poll(time.Second, 10*time.Second, func() (bool, error) {
		// Get all secrets in the namespace
		secrets, err := c.Legacy().Secrets(ns).List(api.ListOptions{})
		// Retrieval errors should fail
		if err != nil {
			return false, err
		}
		for _, s := range secrets.Items {
			if tokensToCleanup.Has(s.Name) {
				// Still waiting for tokens to be cleaned up
				return false, nil
			}
		}
		// All clean
		return true, nil
	})
	if err != nil {
		t.Fatalf("Error waiting for tokens to be deleted: %v", err)
	}
}

func TestServiceAccountTokenAutoMount(t *testing.T) {
	c, _, stopFunc := startServiceAccountTestServer(t)
	defer stopFunc()

	ns := "auto-mount-ns"

	// Create "my" namespace
	_, err := c.Legacy().Namespaces().Create(&api.Namespace{ObjectMeta: api.ObjectMeta{Name: ns}})
	if err != nil && !errors.IsAlreadyExists(err) {
		t.Fatalf("could not create namespace: %v", err)
	}

	// Get default token
	defaultTokenName, _, err := getReferencedServiceAccountToken(c, ns, serviceaccountadmission.DefaultServiceAccountName, true)
	if err != nil {
		t.Fatal(err)
	}

	// Pod to create
	protoPod := api.Pod{
		ObjectMeta: api.ObjectMeta{Name: "protopod"},
		Spec: api.PodSpec{
			Containers: []api.Container{
				{
					Name:  "container-1",
					Image: "container-1-image",
				},
				{
					Name:  "container-2",
					Image: "container-2-image",
					VolumeMounts: []api.VolumeMount{
						{Name: "empty-dir", MountPath: serviceaccountadmission.DefaultAPITokenMountPath},
					},
				},
			},
			Volumes: []api.Volume{
				{
					Name:         "empty-dir",
					VolumeSource: api.VolumeSource{EmptyDir: &api.EmptyDirVolumeSource{}},
				},
			},
		},
	}

	// Pod we expect to get created
	expectedServiceAccount := serviceaccountadmission.DefaultServiceAccountName
	expectedVolumes := append(protoPod.Spec.Volumes, api.Volume{
		Name: defaultTokenName,
		VolumeSource: api.VolumeSource{
			Secret: &api.SecretVolumeSource{
				SecretName: defaultTokenName,
			},
		},
	})
	expectedContainer1VolumeMounts := []api.VolumeMount{
		{Name: defaultTokenName, MountPath: serviceaccountadmission.DefaultAPITokenMountPath, ReadOnly: true},
	}
	expectedContainer2VolumeMounts := protoPod.Spec.Containers[1].VolumeMounts

	createdPod, err := c.Legacy().Pods(ns).Create(&protoPod)
	if err != nil {
		t.Fatal(err)
	}
	if createdPod.Spec.ServiceAccountName != expectedServiceAccount {
		t.Fatalf("Expected %s, got %s", expectedServiceAccount, createdPod.Spec.ServiceAccountName)
	}
	if !api.Semantic.DeepEqual(&expectedVolumes, &createdPod.Spec.Volumes) {
		t.Fatalf("Expected\n\t%#v\n\tgot\n\t%#v", expectedVolumes, createdPod.Spec.Volumes)
	}
	if !api.Semantic.DeepEqual(&expectedContainer1VolumeMounts, &createdPod.Spec.Containers[0].VolumeMounts) {
		t.Fatalf("Expected\n\t%#v\n\tgot\n\t%#v", expectedContainer1VolumeMounts, createdPod.Spec.Containers[0].VolumeMounts)
	}
	if !api.Semantic.DeepEqual(&expectedContainer2VolumeMounts, &createdPod.Spec.Containers[1].VolumeMounts) {
		t.Fatalf("Expected\n\t%#v\n\tgot\n\t%#v", expectedContainer2VolumeMounts, createdPod.Spec.Containers[1].VolumeMounts)
	}
}

func TestServiceAccountTokenAuthentication(t *testing.T) {
	c, config, stopFunc := startServiceAccountTestServer(t)
	defer stopFunc()

	myns := "auth-ns"
	otherns := "other-ns"

	// Create "my" namespace
	_, err := c.Legacy().Namespaces().Create(&api.Namespace{ObjectMeta: api.ObjectMeta{Name: myns}})
	if err != nil && !errors.IsAlreadyExists(err) {
		t.Fatalf("could not create namespace: %v", err)
	}

	// Create "other" namespace
	_, err = c.Legacy().Namespaces().Create(&api.Namespace{ObjectMeta: api.ObjectMeta{Name: otherns}})
	if err != nil && !errors.IsAlreadyExists(err) {
		t.Fatalf("could not create namespace: %v", err)
	}

	// Create "ro" user in myns
	_, err = c.Legacy().ServiceAccounts(myns).Create(&api.ServiceAccount{ObjectMeta: api.ObjectMeta{Name: readOnlyServiceAccountName}})
	if err != nil {
		t.Fatalf("Service Account not created: %v", err)
	}
	roTokenName, roToken, err := getReferencedServiceAccountToken(c, myns, readOnlyServiceAccountName, true)
	if err != nil {
		t.Fatal(err)
	}
	roClientConfig := config
	roClientConfig.BearerToken = roToken
	roClient := clientset.NewForConfigOrDie(&roClientConfig)
	doServiceAccountAPIRequests(t, roClient, myns, true, true, false)
	doServiceAccountAPIRequests(t, roClient, otherns, true, false, false)
	err = c.Legacy().Secrets(myns).Delete(roTokenName, nil)
	if err != nil {
		t.Fatalf("could not delete token: %v", err)
	}
	doServiceAccountAPIRequests(t, roClient, myns, false, false, false)

	// Create "rw" user in myns
	_, err = c.Legacy().ServiceAccounts(myns).Create(&api.ServiceAccount{ObjectMeta: api.ObjectMeta{Name: readWriteServiceAccountName}})
	if err != nil {
		t.Fatalf("Service Account not created: %v", err)
	}
	_, rwToken, err := getReferencedServiceAccountToken(c, myns, readWriteServiceAccountName, true)
	if err != nil {
		t.Fatal(err)
	}
	rwClientConfig := config
	rwClientConfig.BearerToken = rwToken
	rwClient := clientset.NewForConfigOrDie(&rwClientConfig)
	doServiceAccountAPIRequests(t, rwClient, myns, true, true, true)
	doServiceAccountAPIRequests(t, rwClient, otherns, true, false, false)

	// Get default user and token which should have been automatically created
	_, defaultToken, err := getReferencedServiceAccountToken(c, myns, "default", true)
	if err != nil {
		t.Fatalf("could not get default user and token: %v", err)
	}
	defaultClientConfig := config
	defaultClientConfig.BearerToken = defaultToken
	defaultClient := clientset.NewForConfigOrDie(&defaultClientConfig)
	doServiceAccountAPIRequests(t, defaultClient, myns, true, false, false)
}

// startServiceAccountTestServer returns a started server
// It is the responsibility of the caller to ensure the returned stopFunc is called
func startServiceAccountTestServer(t *testing.T) (*clientset.Clientset, client.Config, func()) {

	deleteAllEtcdKeys()

	// Listener
	var m *master.Master
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		m.Handler.ServeHTTP(w, req)
	}))

	// Anonymous client config
	clientConfig := client.Config{Host: apiServer.URL, ContentConfig: client.ContentConfig{GroupVersion: testapi.Default.GroupVersion()}}
	// Root client
	// TODO: remove rootClient after we refactor pkg/admission to use the clientset.
	rootClientset := clientset.NewForConfigOrDie(&client.Config{Host: apiServer.URL, ContentConfig: client.ContentConfig{GroupVersion: testapi.Default.GroupVersion()}, BearerToken: rootToken})
	// Set up two authenticators:
	// 1. A token authenticator that maps the rootToken to the "root" user
	// 2. A ServiceAccountToken authenticator that validates ServiceAccount tokens
	rootTokenAuth := authenticator.TokenFunc(func(token string) (user.Info, bool, error) {
		if token == rootToken {
			return &user.DefaultInfo{rootUserName, "", []string{}}, true, nil
		}
		return nil, false, nil
	})
	serviceAccountKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	serviceAccountTokenGetter := serviceaccountcontroller.NewGetterFromClient(rootClientset)
	serviceAccountTokenAuth := serviceaccount.JWTTokenAuthenticator([]*rsa.PublicKey{&serviceAccountKey.PublicKey}, true, serviceAccountTokenGetter)
	authenticator := union.New(
		bearertoken.New(rootTokenAuth),
		bearertoken.New(serviceAccountTokenAuth),
	)

	// Set up a stub authorizer:
	// 1. The "root" user is allowed to do anything
	// 2. ServiceAccounts named "ro" are allowed read-only operations in their namespace
	// 3. ServiceAccounts named "rw" are allowed any operation in their namespace
	authorizer := authorizer.AuthorizerFunc(func(attrs authorizer.Attributes) error {
		username := attrs.GetUserName()
		ns := attrs.GetNamespace()

		// If the user is "root"...
		if username == rootUserName {
			// allow them to do anything
			return nil
		}

		// If the user is a service account...
		if serviceAccountNamespace, serviceAccountName, err := serviceaccount.SplitUsername(username); err == nil {
			// Limit them to their own namespace
			if serviceAccountNamespace == ns {
				switch serviceAccountName {
				case readOnlyServiceAccountName:
					if attrs.IsReadOnly() {
						return nil
					}
				case readWriteServiceAccountName:
					return nil
				}
			}
		}

		return fmt.Errorf("User %s is denied (ns=%s, readonly=%v, resource=%s)", username, ns, attrs.IsReadOnly(), attrs.GetResource())
	})

	// Set up admission plugin to auto-assign serviceaccounts to pods
	serviceAccountAdmission := serviceaccountadmission.NewServiceAccount(rootClientset)

	masterConfig := framework.NewMasterConfig()
	masterConfig.EnableIndex = true
	masterConfig.Authenticator = authenticator
	masterConfig.Authorizer = authorizer
	masterConfig.AdmissionControl = serviceAccountAdmission

	// Create a master and install handlers into mux.
	m, err := master.New(masterConfig)
	if err != nil {
		t.Fatalf("Error in bringing up the master: %v", err)
	}

	// Start the service account and service account token controllers
	tokenController := serviceaccountcontroller.NewTokensController(rootClientset, serviceaccountcontroller.TokensControllerOptions{TokenGenerator: serviceaccount.JWTTokenGenerator(serviceAccountKey)})
	tokenController.Run()
	serviceAccountController := serviceaccountcontroller.NewServiceAccountsController(rootClientset, serviceaccountcontroller.DefaultServiceAccountsControllerOptions())
	serviceAccountController.Run()
	// Start the admission plugin reflectors
	serviceAccountAdmission.Run()

	stop := func() {
		tokenController.Stop()
		serviceAccountController.Stop()
		serviceAccountAdmission.Stop()
		// TODO: Uncomment when fix #19254
		// apiServer.Close()
	}

	return rootClientset, clientConfig, stop
}

func getServiceAccount(c *clientset.Clientset, ns string, name string, shouldWait bool) (*api.ServiceAccount, error) {
	if !shouldWait {
		return c.Legacy().ServiceAccounts(ns).Get(name)
	}

	var user *api.ServiceAccount
	var err error
	err = wait.Poll(time.Second, 10*time.Second, func() (bool, error) {
		user, err = c.Legacy().ServiceAccounts(ns).Get(name)
		if errors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, nil
	})
	return user, err
}

func getReferencedServiceAccountToken(c *clientset.Clientset, ns string, name string, shouldWait bool) (string, string, error) {
	tokenName := ""
	token := ""

	findToken := func() (bool, error) {
		user, err := c.Legacy().ServiceAccounts(ns).Get(name)
		if errors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}

		for _, ref := range user.Secrets {
			secret, err := c.Legacy().Secrets(ns).Get(ref.Name)
			if errors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return false, err
			}
			if secret.Type != api.SecretTypeServiceAccountToken {
				continue
			}
			name := secret.Annotations[api.ServiceAccountNameKey]
			uid := secret.Annotations[api.ServiceAccountUIDKey]
			tokenData := secret.Data[api.ServiceAccountTokenKey]
			if name == user.Name && uid == string(user.UID) && len(tokenData) > 0 {
				tokenName = secret.Name
				token = string(tokenData)
				return true, nil
			}
		}

		return false, nil
	}

	if shouldWait {
		err := wait.Poll(time.Second, 10*time.Second, findToken)
		if err != nil {
			return "", "", err
		}
	} else {
		ok, err := findToken()
		if err != nil {
			return "", "", err
		}
		if !ok {
			return "", "", fmt.Errorf("No token found for %s/%s", ns, name)
		}
	}
	return tokenName, token, nil
}

type testOperation func() error

func doServiceAccountAPIRequests(t *testing.T, c *clientset.Clientset, ns string, authenticated bool, canRead bool, canWrite bool) {
	testSecret := &api.Secret{
		ObjectMeta: api.ObjectMeta{Name: "testSecret"},
		Data:       map[string][]byte{"test": []byte("data")},
	}

	readOps := []testOperation{
		func() error {
			_, err := c.Legacy().Secrets(ns).List(api.ListOptions{})
			return err
		},
		func() error {
			_, err := c.Legacy().Pods(ns).List(api.ListOptions{})
			return err
		},
	}
	writeOps := []testOperation{
		func() error { _, err := c.Legacy().Secrets(ns).Create(testSecret); return err },
		func() error { return c.Legacy().Secrets(ns).Delete(testSecret.Name, nil) },
	}

	for _, op := range readOps {
		err := op()
		unauthorizedError := errors.IsUnauthorized(err)
		forbiddenError := errors.IsForbidden(err)

		switch {
		case !authenticated && !unauthorizedError:
			t.Fatalf("expected unauthorized error, got %v", err)
		case authenticated && unauthorizedError:
			t.Fatalf("unexpected unauthorized error: %v", err)
		case authenticated && canRead && forbiddenError:
			t.Fatalf("unexpected forbidden error: %v", err)
		case authenticated && !canRead && !forbiddenError:
			t.Fatalf("expected forbidden error, got: %v", err)
		}
	}

	for _, op := range writeOps {
		err := op()
		unauthorizedError := errors.IsUnauthorized(err)
		forbiddenError := errors.IsForbidden(err)

		switch {
		case !authenticated && !unauthorizedError:
			t.Fatalf("expected unauthorized error, got %v", err)
		case authenticated && unauthorizedError:
			t.Fatalf("unexpected unauthorized error: %v", err)
		case authenticated && canWrite && forbiddenError:
			t.Fatalf("unexpected forbidden error: %v", err)
		case authenticated && !canWrite && !forbiddenError:
			t.Fatalf("expected forbidden error, got: %v", err)
		}
	}
}
