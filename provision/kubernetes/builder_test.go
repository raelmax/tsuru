// Copyright 2018 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package kubernetes

import (
	"io"
	"io/ioutil"
	"net/http"
	"regexp"
	"sort"
	"strings"

	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kfake "k8s.io/client-go/kubernetes/typed/core/v1/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/tsuru/config"
	"github.com/tsuru/tsuru/event"
	"github.com/tsuru/tsuru/permission"
	"gopkg.in/check.v1"
)

func (s *S) TestBuildPod(c *check.C) {
	a, _, rollback := s.mock.DefaultReactions(c)
	defer rollback()
	fakePods, ok := s.client.Core().Pods(s.client.Namespace()).(*kfake.FakePods)
	c.Assert(ok, check.Equals, true)
	fakePods.Fake.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		pod := action.(ktesting.CreateAction).GetObject().(*apiv1.Pod)
		containers := pod.Spec.Containers
		c.Assert(containers, check.HasLen, 2)
		sort.Slice(containers, func(i, j int) bool { return containers[i].Name < containers[j].Name })
		cmds := cleanCmds(containers[0].Command[2])
		c.Assert(cmds, check.Equals, `end() { touch /tmp/intercontainer/done; }
trap end EXIT
while [ ! -f /tmp/intercontainer/status ]; do sleep 1; done
exit_code=$(cat /tmp/intercontainer/status)
[ "${exit_code}" != "0" ] && exit "${exit_code}"
id=$(docker ps -aq -f "label=io.kubernetes.container.name=myapp-v1-build" -f "label=io.kubernetes.pod.name=$(hostname)")
img="tsuru/app-myapp:mytag"
echo
echo '---- Building application image ----'
docker commit "${id}" "${img}" >/dev/null
sz=$(docker history "${img}" | head -2 | tail -1 | grep -E -o '[0-9.]+\s[a-zA-Z]+\s*$' | sed 's/[[:space:]]*$//g')
echo " ---> Sending image to repository (${sz})"
docker push tsuru/app-myapp:mytag`)
		return false, nil, nil
	})
	evt, err := event.New(&event.Opts{
		Target:  event.Target{Type: event.TargetTypeApp, Value: a.GetName()},
		Kind:    permission.PermAppDeploy,
		Owner:   s.token,
		Allowed: event.Allowed(permission.PermAppDeploy),
	})
	c.Assert(err, check.IsNil)
	buf := strings.NewReader("my upload data")
	client := KubeClient{}
	_, err = client.BuildPod(a, evt, ioutil.NopCloser(buf), "mytag")
	c.Assert(err, check.IsNil)
}

func (s *S) TestImageTagPushAndInspect(c *check.C) {
	s.mock.LogHook = func(w io.Writer, r *http.Request) {
		exp := regexp.MustCompile("/api/v1/namespaces/default/pods/(.*)/attach")
		parts := exp.FindStringSubmatch(r.URL.Path)
		c.Assert(parts, check.HasLen, 2)
		switch parts[1] {
		case "myapp-v1-deploy":
			w.Write([]byte(`[{"Id":"1234"}]`))
		case "myapp-v1-build-procfile-inspect":
			w.Write([]byte(`web: make run`))
		case "myapp-v1-build-yamldata":
			w.Write([]byte("healthcheck:\n  path: /health\n  scheme: https"))
		}
	}
	a, _, rollback := s.mock.DefaultReactions(c)
	defer rollback()
	client := KubeClient{}
	img, procfileRaw, yamlData, err := client.ImageTagPushAndInspect(a, "tsuru/app-myapp:tag1", "tsuru/app-myapp:tag2")
	c.Assert(err, check.IsNil)
	c.Assert(img.ID, check.Equals, "1234")
	c.Assert(procfileRaw, check.Equals, "web: make run")
	c.Assert(yamlData.Healthcheck.Path, check.Equals, "/health")
	c.Assert(yamlData.Healthcheck.Scheme, check.Equals, "https")
}

func (s *S) TestImageTagPushAndInspectWithRegistryAuth(c *check.C) {
	config.Set("docker:registry", "registry.example.com")
	defer config.Unset("docker:registry")
	config.Set("docker:registry-auth:username", "user")
	defer config.Unset("docker:registry-auth:username")
	config.Set("docker:registry-auth:password", "pwd")
	defer config.Unset("docker:registry-auth:password")

	s.mock.LogHook = func(w io.Writer, r *http.Request) {
		exp := regexp.MustCompile("/api/v1/namespaces/default/pods/(.*)/attach")
		parts := exp.FindStringSubmatch(r.URL.Path)
		c.Assert(parts, check.HasLen, 2)
		switch parts[1] {
		case "myapp-v1-deploy":
			w.Write([]byte(`[{"Id":"1234"}]`))
		case "myapp-v1-build-procfile-inspect":
			w.Write([]byte(`web: make run`))
		case "myapp-v1-build-yamldata":
			w.Write([]byte("healthcheck:\n  path: /health\n  scheme: https"))
		}
	}
	a, _, rollback := s.mock.DefaultReactions(c)
	defer rollback()
	fakePods, ok := s.client.Core().Pods(s.client.Namespace()).(*kfake.FakePods)
	c.Assert(ok, check.Equals, true)
	fakePods.Fake.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		pod := action.(ktesting.CreateAction).GetObject().(*apiv1.Pod)
		containers := pod.Spec.Containers
		if containers[0].Name == "myapp-v1-deploy" {
			c.Assert(containers, check.HasLen, 1)
			cmds := cleanCmds(containers[0].Command[2])
			c.Assert(cmds, check.Equals, `cat >/dev/null &&
docker login -u "user" -p "pwd" "registry.example.com"
docker pull registry.example.com/tsuru/app-myapp:tag1 >/dev/null
docker inspect registry.example.com/tsuru/app-myapp:tag1
docker tag registry.example.com/tsuru/app-myapp:tag1 registry.example.com/tsuru/app-myapp:tag2
docker login -u "user" -p "pwd" "registry.example.com"
docker push registry.example.com/tsuru/app-myapp:tag2`)
		}
		return false, nil, nil
	})

	client := KubeClient{}
	img, procfileRaw, yamlData, err := client.ImageTagPushAndInspect(a, "registry.example.com/tsuru/app-myapp:tag1", "registry.example.com/tsuru/app-myapp:tag2")
	c.Assert(err, check.IsNil)
	c.Assert(img.ID, check.Equals, "1234")
	c.Assert(procfileRaw, check.Equals, "web: make run")
	c.Assert(yamlData.Healthcheck.Path, check.Equals, "/health")
	c.Assert(yamlData.Healthcheck.Scheme, check.Equals, "https")
}

func (s *S) TestImageTagPushAndInspectWithRegistryAuthAndDifferentDomain(c *check.C) {
	config.Set("docker:registry", "registry.example.com")
	defer config.Unset("docker:registry")
	config.Set("docker:registry-auth:username", "user")
	defer config.Unset("docker:registry-auth:username")
	config.Set("docker:registry-auth:password", "pwd")
	defer config.Unset("docker:registry-auth:password")

	s.mock.LogHook = func(w io.Writer, r *http.Request) {
		exp := regexp.MustCompile("/api/v1/namespaces/default/pods/(.*)/attach")
		parts := exp.FindStringSubmatch(r.URL.Path)
		c.Assert(parts, check.HasLen, 2)
		switch parts[1] {
		case "myapp-v1-deploy":
			w.Write([]byte(`[{"Id":"1234"}]`))
		case "myapp-v1-build-procfile-inspect":
			w.Write([]byte(`web: make run`))
		case "myapp-v1-build-yamldata":
			w.Write([]byte("healthcheck:\n  path: /health\n  scheme: https"))
		}
	}
	a, _, rollback := s.mock.DefaultReactions(c)
	defer rollback()
	fakePods, ok := s.client.Core().Pods(s.client.Namespace()).(*kfake.FakePods)
	c.Assert(ok, check.Equals, true)
	fakePods.Fake.PrependReactor("create", "pods", func(action ktesting.Action) (handled bool, ret runtime.Object, err error) {
		pod := action.(ktesting.CreateAction).GetObject().(*apiv1.Pod)
		containers := pod.Spec.Containers
		if containers[0].Name == "myapp-v1-deploy" {
			pod := action.(ktesting.CreateAction).GetObject().(*apiv1.Pod)
			containers := pod.Spec.Containers
			c.Assert(containers, check.HasLen, 1)
			cmds := cleanCmds(containers[0].Command[2])
			c.Assert(cmds, check.Equals, `cat >/dev/null &&
docker pull otherregistry.example.com/tsuru/app-myapp:tag1 >/dev/null
docker inspect otherregistry.example.com/tsuru/app-myapp:tag1
docker tag otherregistry.example.com/tsuru/app-myapp:tag1 otherregistry.example.com/tsuru/app-myapp:tag2
docker push otherregistry.example.com/tsuru/app-myapp:tag2`)
		}
		return false, nil, nil
	})

	client := KubeClient{}
	img, procfileRaw, yamlData, err := client.ImageTagPushAndInspect(a, "otherregistry.example.com/tsuru/app-myapp:tag1", "otherregistry.example.com/tsuru/app-myapp:tag2")
	c.Assert(err, check.IsNil)
	c.Assert(img.ID, check.Equals, "1234")
	c.Assert(procfileRaw, check.Equals, "web: make run")
	c.Assert(yamlData.Healthcheck.Path, check.Equals, "/health")
	c.Assert(yamlData.Healthcheck.Scheme, check.Equals, "https")
}
