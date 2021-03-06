// Copyright 2020 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file  except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the  License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provisioners

import (
	"crypto/rand"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	v1 "github.com/couchbase/service-broker/pkg/apis/servicebroker/v1alpha1"
	"github.com/couchbase/service-broker/pkg/config"
	"github.com/couchbase/service-broker/pkg/errors"
	"github.com/couchbase/service-broker/pkg/log"
	"github.com/couchbase/service-broker/pkg/registry"
	"github.com/couchbase/service-broker/pkg/util"

	"github.com/evanphx/json-patch"
	"github.com/go-openapi/jsonpointer"
	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/runtime"
)

// GetNamespace returns the namespace to provision resources in.  This is the namespace
// the broker lives in by default, however when operating as a kubernetes cluster service
// broker then this information is passed as request context.
func GetNamespace(context *runtime.RawExtension) (string, error) {
	if context != nil {
		var ctx interface{}

		if err := json.Unmarshal(context.Raw, &ctx); err != nil {
			glog.Infof("unmarshal of client context failed: %v", err)
			return "", err
		}

		pointer, err := jsonpointer.New("/namespace")
		if err != nil {
			glog.Infof("failed to parse JSON pointer: %v", err)
			return "", err
		}

		v, _, err := pointer.Get(ctx)
		if err == nil {
			namespace, ok := v.(string)
			if ok {
				return namespace, nil
			}

			glog.Infof("request context namespace not a string")

			return "", errors.NewParameterError("request context namespace not a string")
		}
	}

	return config.Namespace(), nil
}

// getServiceAndPlanNames translates from GUIDs to human readable names used in configuration.
func getServiceAndPlanNames(serviceID, planID string) (string, string, error) {
	for _, service := range config.Config().Spec.Catalog.Services {
		if service.ID == serviceID {
			for _, plan := range service.Plans {
				if plan.ID == planID {
					return service.Name, plan.Name, nil
				}
			}

			return "", "", fmt.Errorf("unable to locate plan for ID %s", planID)
		}
	}

	return "", "", fmt.Errorf("unable to locate service for ID %s", serviceID)
}

// getTemplateBindings returns the template bindings associated with a creation request's
// service and plan IDs.
func getTemplateBindings(serviceID, planID string) (*v1.ConfigurationBinding, error) {
	service, plan, err := getServiceAndPlanNames(serviceID, planID)
	if err != nil {
		return nil, err
	}

	for index, binding := range config.Config().Spec.Bindings {
		if binding.Service == service && binding.Plan == plan {
			return &config.Config().Spec.Bindings[index], nil
		}
	}

	return nil, fmt.Errorf("unable to locate template bindings for service plan %s/%s", service, plan)
}

// getTemplateBinding returns the binding associated with a specific resource type.
func getTemplateBinding(t ResourceType, serviceID, planID string) (*v1.ServiceBrokerTemplateList, error) {
	bindings, err := getTemplateBindings(serviceID, planID)
	if err != nil {
		return nil, err
	}

	var templates *v1.ServiceBrokerTemplateList

	switch t {
	case ResourceTypeServiceInstance:
		templates = &bindings.ServiceInstance
	case ResourceTypeServiceBinding:
		templates = bindings.ServiceBinding
	default:
		return nil, fmt.Errorf("illegal binding type %s", string(t))
	}

	if templates == nil {
		return nil, errors.NewConfigurationError("missing bindings for type %s", string(t))
	}

	return templates, nil
}

// getTemplate returns the template corresponding to a template name.
func getTemplate(name string) (*v1.ConfigurationTemplate, error) {
	for index, template := range config.Config().Spec.Templates {
		if template.Name == name {
			return &config.Config().Spec.Templates[index], nil
		}
	}

	return nil, fmt.Errorf("unable to locate template for %s", name)
}

// resolveParameter attempts to find the parameter path in the provided JSON.
func resolveParameter(path string, entry *registry.Entry) (interface{}, bool, error) {
	var parameters interface{}

	ok, err := entry.Get(registry.Parameters, &parameters)
	if err != nil {
		return nil, false, err
	}

	if !ok {
		return nil, false, fmt.Errorf("unable to lookup parameters")
	}

	pointer, err := jsonpointer.New(path)
	if err != nil {
		glog.Infof("failed to parse JSON pointer: %v", err)
		return nil, false, err
	}

	value, _, err := pointer.Get(parameters)
	if err != nil {
		return nil, false, nil
	}

	return value, true, nil
}

// resolveAccessor looks up a registry or parameter.
func resolveAccessor(accessor *v1.Accessor, entry *registry.Entry) (interface{}, error) {
	var value interface{}

	switch {
	case accessor.Registry != nil:
		v, ok, err := entry.GetUser(*accessor.Registry)
		if err != nil {
			return nil, err
		}

		if !ok {
			return nil, nil
		}

		value = v

	case accessor.Parameter != nil:
		v, ok, err := resolveParameter(*accessor.Parameter, entry)
		if err != nil {
			return nil, err
		}

		if !ok {
			return nil, nil
		}

		value = v
	default:
		return nil, fmt.Errorf("accessor must have one method defined")
	}

	glog.Infof("resolved parameter value %v", value)

	return value, nil
}

// resolveString looks up a string value.
func resolveString(str *v1.String, entry *registry.Entry) (string, error) {
	if str.String != nil {
		return *str.String, nil
	}

	value, err := resolveAccessor(&str.Accessor, entry)
	if err != nil {
		return "", err
	}

	stringValue, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("value %v is not a string", value)
	}

	return stringValue, nil
}

// resolveAccessorStringList looks up a list of parameters trhrowing an error if
// any member is not a string.
func resolveAccessorStringList(accessors []v1.Accessor, entry *registry.Entry) ([]string, error) {
	list := []string{}

	for index := range accessors {
		value, err := resolveAccessor(&accessors[index], entry)
		if err != nil {
			return nil, err
		}

		if value == nil {
			continue
		}

		strValue, ok := value.(string)
		if !ok {
			return nil, errors.NewConfigurationError("accessor string list element not a string %v", value)
		}

		list = append(list, strValue)
	}

	return list, nil
}

// resolveFormat attepts to render a format parameter.
func resolveFormat(format *v1.ConfigurationParameterSourceFormat, entry *registry.Entry) (interface{}, error) {
	parameters := make([]interface{}, len(format.Parameters))

	for index := range format.Parameters {
		value, err := resolveAccessor(&format.Parameters[index], entry)
		if err != nil {
			return nil, err
		}

		if value == nil {
			return nil, nil
		}

		parameters[index] = value
	}

	return fmt.Sprintf(format.String, parameters...), nil
}

// resolveGeneratePassword generates a random string.
func resolveGeneratePassword(config *v1.ConfigurationParameterSourceGeneratePassword) (interface{}, error) {
	dictionary := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if config.Dictionary != nil {
		dictionary = *config.Dictionary
	}

	glog.Infof("generating string with length %d using dictionary %s", config.Length, dictionary)

	// Adjust so the length is within array bounds.
	arrayIndexOffset := 1
	dictionaryLength := len(dictionary) - arrayIndexOffset

	limit := big.NewInt(int64(dictionaryLength))
	value := ""

	for i := 0; i < config.Length; i++ {
		indexBig, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return nil, err
		}

		if !indexBig.IsInt64() {
			return nil, errors.NewConfigurationError("random index overflow")
		}

		index := int(indexBig.Int64())

		value += dictionary[index : index+1]
	}

	return value, nil
}

// resolveGenerateKey will create a PEM encoded private key.
func resolveGenerateKey(config *v1.ConfigurationParameterSourceGenerateKey) (interface{}, error) {
	key, err := util.GenerateKey(config.Type, config.Encoding, config.Bits)
	if err != nil {
		return nil, err
	}

	return string(key), nil
}

// getPrivateKey reads a PEM formatted private key.
func getPrivateKey(parameter *v1.Accessor, entry *registry.Entry) ([]byte, error) {
	keyPEM, err := resolveAccessor(parameter, entry)
	if err != nil {
		return nil, err
	}

	data, ok := keyPEM.(string)
	if !ok {
		return nil, errors.NewConfigurationError("private key is not a string")
	}

	return []byte(data), nil
}

// getCertificate resolves a PEM formatted certificate.
func getCertificate(parameter *v1.Accessor, entry *registry.Entry) ([]byte, error) {
	certPEM, err := resolveAccessor(parameter, entry)
	if err != nil {
		return nil, err
	}

	data, ok := certPEM.(string)
	if !ok {
		return nil, errors.NewConfigurationError("certificate is not a string")
	}

	return []byte(data), nil
}

// resolveGenerateCertificate will create a PEM encoded X.509 certificate.
func resolveGenerateCertificate(config *v1.ConfigurationParameterSourceGenerateCertificate, entry *registry.Entry) (interface{}, error) {
	key, err := getPrivateKey(&config.Key, entry)
	if err != nil {
		return nil, err
	}

	subject := pkix.Name{
		CommonName: config.Subject.CommonName,
	}

	// Resolve SANs if defined.
	var dnsSANs []string

	var emailSANS []string

	if config.AlternativeNames != nil {
		list, err := resolveAccessorStringList(config.AlternativeNames.DNS, entry)
		if err != nil {
			return nil, err
		}

		dnsSANs = list

		list, err = resolveAccessorStringList(config.AlternativeNames.Email, entry)
		if err != nil {
			return nil, err
		}

		emailSANS = list
	}

	// Resolve CA key pair if defined.
	var caCertificate []byte

	var caKey []byte

	if config.CA != nil {
		caKey, err = getPrivateKey(&config.CA.Key, entry)
		if err != nil {
			return nil, err
		}

		caCertificate, err = getCertificate(&config.CA.Certificate, entry)
		if err != nil {
			return nil, err
		}
	}

	cert, err := util.GenerateCertificate(key, subject, config.Lifetime.Duration, config.Usage, dnsSANs, emailSANS, caKey, caCertificate)
	if err != nil {
		return nil, err
	}

	return string(cert), nil
}

// resolveSource gets a parameter source from either metadata or a JSON path into user specified parameters.
func resolveSource(source *v1.ConfigurationParameterSource, entry *registry.Entry) (interface{}, error) {
	if source == nil {
		return nil, nil
	}

	// Try to resolve the parameter.
	var value interface{}

	switch {
	// Registry parameters are reference registry values.
	case source.Accessor.Registry != nil:
		key := *source.Accessor.Registry

		glog.Infof("getting registry key %s", key)

		v, ok, err := entry.GetUser(key)
		if err != nil {
			return nil, err
		}

		if !ok {
			glog.Infof("registry key %s not set, skipping", key)
			break
		}

		value = v

	// Parameters reference parameters specified by the client in response to
	// the schema advertised to the client.
	case source.Accessor.Parameter != nil:
		path := *source.Accessor.Parameter

		glog.Infof("getting parameter for path %s", path)

		v, ok, err := resolveParameter(path, entry)
		if err != nil {
			return nil, err
		}

		if !ok {
			glog.Infof("client parameter %s not set, skipping", path)
			break
		}

		value = v

	// Format will do a string formatting with Sprintf and a variadic set
	// of parameters.
	case source.Format != nil:
		v, err := resolveFormat(source.Format, entry)
		if err != nil {
			return nil, err
		}

		value = v

	// GeneratePassword will randomly generate a password string for example.
	case source.GeneratePassword != nil:
		v, err := resolveGeneratePassword(source.GeneratePassword)
		if err != nil {
			return nil, err
		}

		value = v

	// GenerateKey will create a PEM encoded private key.
	case source.GenerateKey != nil:
		v, err := resolveGenerateKey(source.GenerateKey)
		if err != nil {
			return nil, err
		}

		value = v

	// GenerateCertificate will create a PEM encoded X.509 certificate.
	case source.GenerateCertificate != nil:
		v, err := resolveGenerateCertificate(source.GenerateCertificate, entry)
		if err != nil {
			return nil, err
		}

		value = v

	// Template will recursively render a template and return an object.
	// This allows sharing of common configuration.
	case source.Template != nil:
		t, err := getTemplate(*source.Template)
		if err != nil {
			return nil, err
		}

		template, err := renderTemplate(t, entry)
		if err != nil {
			return nil, err
		}

		var v interface{}
		if err := json.Unmarshal(template.Template.Raw, &v); err != nil {
			glog.Infof("unmarshal of template failed: %v", err)
			return nil, err
		}

		value = v
	}

	glog.Infof("resolved source value %v", value)

	return value, nil
}

// resolveTemplateParameter applies parameter lookup rules and tries to return a value.
func resolveTemplateParameter(parameter *v1.ConfigurationParameter, entry *registry.Entry) (interface{}, error) {
	value, err := resolveSource(parameter.Source, entry)
	if err != nil {
		return nil, err
	}

	// If no value has been found or generated then use a default if set.
	if value == nil && parameter.Default != nil {
		switch {
		case parameter.Default.String != nil:
			value = *parameter.Default.String
		case parameter.Default.Bool != nil:
			value = *parameter.Default.Bool
		case parameter.Default.Int != nil:
			value = *parameter.Default.Int
		case parameter.Default.Object != nil:
			var v interface{}

			if err := json.Unmarshal(parameter.Default.Object.Raw, &v); err != nil {
				glog.Infof("unmarshal of source default failed: %v", err)
				return nil, err
			}

			value = v
		default:
			return nil, errors.NewConfigurationError("undefined source default parameter")
		}

		glog.Infof("using source default %v", value)
	}

	if parameter.Required && value == nil {
		glog.Infof("source unset but is required")
		return nil, errors.NewParameterError("source for parameter %s is required", parameter.Name)
	}

	return value, nil
}

// patchObject takes a raw JSON object and applies parameters to it.
func patchObject(object []byte, parameters []v1.ConfigurationParameter, entry *registry.Entry) ([]byte, error) {
	// Now for the fun bit.  Work through each defined parameter and apply it to
	// the object.  This basically works like JSON patch++, automatically filling
	// in parent objects and arrays as necessary.
	for index, parameter := range parameters {
		value, err := resolveTemplateParameter(&parameters[index], entry)
		if err != nil {
			return nil, err
		}

		if value == nil {
			continue
		}

		// Set each destination path using JSON patch.
		patches := []string{}

		for _, destination := range parameter.Destinations {
			switch {
			case destination.Registry != nil:
				strValue, ok := value.(string)
				if !ok {
					return nil, errors.NewConfigurationError("parameter %s is not a string", parameter.Name)
				}

				if err := entry.SetUser(*destination.Registry, strValue); err != nil {
					return nil, errors.NewConfigurationError(err.Error())
				}

			case destination.Path != nil:
				valueJSON, err := json.Marshal(value)
				if err != nil {
					glog.Infof("marshal of value failed: %v", err)
					return nil, err
				}

				patches = append(patches, fmt.Sprintf(`{"op":"add","path":"%s","value":%s}`, *destination.Path, string(valueJSON)))
			}
		}

		minPatches := 1
		if len(patches) < minPatches {
			glog.Infof("no paths to apply parameter to")
			continue
		}

		patchSet := "[" + strings.Join(patches, ",") + "]"

		glog.Infof("applying patchset %s", patchSet)

		patch, err := jsonpatch.DecodePatch([]byte(patchSet))
		if err != nil {
			glog.Infof("decode of JSON patch failed: %v", err)
			return nil, err
		}

		if object, err = patch.Apply(object); err != nil {
			glog.Infof("apply of JSON patch failed: %v", err)
			return nil, err
		}
	}

	return object, nil
}

// renderTemplate accepts a template defined in the configuration and applies any
// request or metadata parameters to it.
func renderTemplate(template *v1.ConfigurationTemplate, entry *registry.Entry) (*v1.ConfigurationTemplate, error) {
	glog.Infof("rendering template %s", template.Name)

	if template.Template == nil || template.Template.Raw == nil {
		return nil, errors.NewConfigurationError("template %s is not defined", template.Name)
	}

	glog.V(log.LevelDebug).Infof("template source: %s", string(template.Template.Raw))

	// We will be modifying the template in place, so first clone it as the
	// config is immutable.
	t := template.DeepCopy()

	object, err := patchObject(t.Template.Raw, t.Parameters, entry)
	if err != nil {
		return nil, err
	}

	t.Template.Raw = object

	glog.Infof("rendered template %s", string(t.Template.Raw))

	return t, nil
}
