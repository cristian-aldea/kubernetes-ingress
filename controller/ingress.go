// Copyright 2019 HAProxy Technologies LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"fmt"

	"github.com/haproxytech/client-native/v2/models"

	"github.com/haproxytech/kubernetes-ingress/controller/annotations"
	"github.com/haproxytech/kubernetes-ingress/controller/haproxy"
	"github.com/haproxytech/kubernetes-ingress/controller/haproxy/rules"
	"github.com/haproxytech/kubernetes-ingress/controller/route"
	"github.com/haproxytech/kubernetes-ingress/controller/service"
	"github.com/haproxytech/kubernetes-ingress/controller/store"
	"github.com/haproxytech/kubernetes-ingress/controller/utils"
)

// igClassIsSupported verifies if the IngressClass matches the ControllerClass
// and in such case returns true otherwise false
//
// According to https://github.com/kubernetes/api/blob/master/networking/v1/types.go#L257
// ingress.class annotation should have precedence over the IngressClass mechanism implemented
// in "networking.k8s.io".
func (c *HAProxyController) igClassIsSupported(ingress *store.Ingress) bool {
	var igClassAnn string
	var igClass *store.IngressClass
	igClassAnn = annotations.GetValue("ingress.class", ingress.Annotations)

	// If ingress class is unassigned and the controller is controlling any resource without explicit ingress class then support it.
	if igClassAnn == "" && c.OSArgs.EmptyIngressClass {
		return true
	}

	if igClassAnn == "" || igClassAnn != c.OSArgs.IngressClass {
		igClass = c.Store.IngressClasses[ingress.Class]
		if igClass != nil && igClass.Status != DELETED && igClass.Controller == CONTROLLER_CLASS {
			// Corresponding IngresClass was updated so Ingress resource should be re-processed
			// This is particularly important if the Ingress was skipped due to mismatching ingrssClass
			if igClass.Status != EMPTY {
				ingress.Status = MODIFIED
			}
			return true
		}
	}
	if igClassAnn == c.OSArgs.IngressClass {
		return true
	}
	return false
}

func (c *HAProxyController) handleIngressPath(ingress *store.Ingress, host string, path *store.IngressPath, ruleIDs []haproxy.RuleID) (reload bool, err error) {
	sslPassthrough := c.sslPassthroughEnabled(*ingress, path)
	svc, err := service.NewCtx(c.Store, ingress, path, sslPassthrough)
	if err != nil {
		return
	}
	if svc.GetStatus() == DELETED {
		return
	}
	// Backend
	backendReload, backendName, err := svc.HandleBackend(c.Client, c.Store)
	if err != nil {
		return
	}
	// Route
	var routeReload bool
	ingRoute := route.Route{
		Host:           host,
		Path:           path,
		HAProxyRules:   ruleIDs,
		BackendName:    backendName,
		SSLPassthrough: sslPassthrough,
	}
	routeACLAnn := annotations.GetValue("route-acl", svc.GetService().Annotations)
	if routeACLAnn == "" {
		if _, ok := route.CustomRoutes[backendName]; ok {
			delete(route.CustomRoutes, backendName)
			logger.Debugf("Custom Route to backend '%s' deleted, reload required", backendName)
			routeReload = true
		}
		err = route.AddHostPathRoute(ingRoute, c.Cfg.MapFiles)
	} else {
		routeReload, err = route.AddCustomRoute(ingRoute, routeACLAnn, c.Client)
	}
	if err != nil {
		return
	}
	c.Cfg.ActiveBackends[backendName] = struct{}{}
	// Endpoints
	endpointsReload := svc.HandleEndpoints(c.Client, c.Store, c.Cfg.Certificates)
	return backendReload || endpointsReload || routeReload, err
}

func (c *HAProxyController) setDefaultService(ingress *store.Ingress, frontends []string) (reload bool, err error) {
	var frontend models.Frontend
	var ftReload bool
	frontend, err = c.Client.FrontendGet(frontends[0])
	if err != nil {
		return
	}
	tcpService := false
	if frontend.Mode == "tcp" {
		tcpService = true
	}
	svc, err := service.NewCtx(c.Store, ingress, ingress.DefaultBackend, tcpService)
	if err != nil {
		return
	}
	if svc.GetStatus() == DELETED {
		return
	}
	bdReload, backendName, err := svc.HandleBackend(c.Client, c.Store)
	if err != nil {
		return
	}
	if frontend.DefaultBackend != backendName {
		if frontend.Name == c.Cfg.FrontHTTP {
			logger.Infof("Setting http default backend to '%s'", backendName)
		}
		for _, frontendName := range frontends {
			frontend, _ := c.Client.FrontendGet(frontendName)
			frontend.DefaultBackend = backendName
			err = c.Client.FrontendEdit(frontend)
			if err != nil {
				return
			}
			ftReload = true
			logger.Debugf("Setting '%s' default backend to '%s'", frontendName, backendName)
		}
	}
	c.Cfg.ActiveBackends[backendName] = struct{}{}
	endpointsReload := svc.HandleEndpoints(c.Client, c.Store, c.Cfg.Certificates)
	reload = bdReload || ftReload || endpointsReload
	return reload, err
}

func (c *HAProxyController) sslPassthroughEnabled(ingress store.Ingress, path *store.IngressPath) bool {
	var annSSLPassthrough string
	var service *store.Service
	ok := false
	if path != nil {
		service, ok = c.Store.Namespaces[ingress.Namespace].Services[path.SvcName]
	}
	if ok {
		annSSLPassthrough = annotations.GetValue("ssl-passthrough", service.Annotations, ingress.Annotations, c.Store.ConfigMaps.Main.Annotations)
	} else {
		annSSLPassthrough = annotations.GetValue("ssl-passthrough", ingress.Annotations, c.Store.ConfigMaps.Main.Annotations)
	}
	if annSSLPassthrough == "" {
		return false
	}
	enabled, err := utils.GetBoolValue(annSSLPassthrough, "ssl-passthrough")
	if err != nil {
		logger.Errorf("ssl-passthrough annotation: %s", err)
		return false
	}
	if enabled {
		c.Cfg.SSLPassthrough = true
		return true
	}
	return false
}

// handleIngressAnnotations processes ingress annotations to create HAProxy Rules and provide
// corresponding list of RuleIDs.
// If Ingress Annotations are at the ConfigMap scope, HAProxy Rules will be applied globally
// without the need to map Rule IDs to specific ingress traffic.
func (c *HAProxyController) handleIngressAnnotations(ingress store.Ingress) []haproxy.RuleID {
	var err error
	var ingressRule bool
	var annValue, annSource string
	var annList map[string]string
	if ingress.Equal(&store.Ingress{}) {
		annSource = "ConfigMap"
		annList = c.Store.ConfigMaps.Main.Annotations
		ingressRule = false
	} else {
		annSource = fmt.Sprintf("Ingress '%s/%s'", ingress.Namespace, ingress.Name)
		annList = ingress.Annotations
		ingressRule = true
	}
	ids := []haproxy.RuleID{}
	frontends := []string{c.Cfg.FrontHTTP, c.Cfg.FrontHTTPS}
	result := haproxy.Rules{}
	for _, a := range annotations.GetFrontendAnnotations(ingress, &result, *c.Cfg.MapFiles, c.Store) {
		annValue = annotations.GetValue(a.GetName(), annList)
		err = a.Process(annValue)
		if err != nil {
			logger.Errorf("%s: annotation %s: %s", annSource, a.GetName(), err)
		}
	}
	for _, rule := range result {
		switch rule.GetType() {
		case haproxy.REQ_REDIRECT:
			redirRule := rule.(*rules.RequestRedirect)
			if redirRule.SSLRedirect {
				frontends = []string{c.Cfg.FrontHTTP}
			} else {
				frontends = []string{c.Cfg.FrontHTTP, c.Cfg.FrontHTTPS}
			}
		case haproxy.REQ_DENY, haproxy.REQ_CAPTURE:
			if c.sslPassthroughEnabled(ingress, nil) {
				frontends = []string{c.Cfg.FrontHTTP, c.Cfg.FrontSSL}
			}
		case haproxy.REQ_RATELIMIT:
			limitRule := rule.(*rules.ReqRateLimit)
			c.Cfg.RateLimitTables = append(c.Cfg.RateLimitTables, limitRule.TableName)
		}
		for _, frontend := range frontends {
			logger.Error(c.Cfg.HAProxyRules.AddRule(rule, ingressRule, frontend))
		}
		ids = append(ids, haproxy.GetID(rule))
	}
	return ids
}
