/*
Copyright The KCP Authors.

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

// Code generated by client-gen. DO NOT EDIT.

package fake

import (
	"context"
	json "encoding/json"
	"fmt"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	types "k8s.io/apimachinery/pkg/types"
	watch "k8s.io/apimachinery/pkg/watch"
	testing "k8s.io/client-go/testing"

	v1alpha1 "github.com/kcp-dev/kcp/test/e2e/fixtures/wildwest/apis/wildwest/v1alpha1"
	wildwestv1alpha1 "github.com/kcp-dev/kcp/test/e2e/fixtures/wildwest/client/applyconfiguration/wildwest/v1alpha1"
)

// FakeSherifves implements SheriffInterface
type FakeSherifves struct {
	Fake *FakeWildwestV1alpha1
}

var sherifvesResource = v1alpha1.SchemeGroupVersion.WithResource("sherifves")

var sherifvesKind = v1alpha1.SchemeGroupVersion.WithKind("Sheriff")

// Get takes name of the sheriff, and returns the corresponding sheriff object, and an error if there is any.
func (c *FakeSherifves) Get(ctx context.Context, name string, options v1.GetOptions) (result *v1alpha1.Sheriff, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootGetAction(sherifvesResource, name), &v1alpha1.Sheriff{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Sheriff), err
}

// List takes label and field selectors, and returns the list of Sherifves that match those selectors.
func (c *FakeSherifves) List(ctx context.Context, opts v1.ListOptions) (result *v1alpha1.SheriffList, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootListAction(sherifvesResource, sherifvesKind, opts), &v1alpha1.SheriffList{})
	if obj == nil {
		return nil, err
	}

	label, _, _ := testing.ExtractFromListOptions(opts)
	if label == nil {
		label = labels.Everything()
	}
	list := &v1alpha1.SheriffList{ListMeta: obj.(*v1alpha1.SheriffList).ListMeta}
	for _, item := range obj.(*v1alpha1.SheriffList).Items {
		if label.Matches(labels.Set(item.Labels)) {
			list.Items = append(list.Items, item)
		}
	}
	return list, err
}

// Watch returns a watch.Interface that watches the requested sherifves.
func (c *FakeSherifves) Watch(ctx context.Context, opts v1.ListOptions) (watch.Interface, error) {
	return c.Fake.
		InvokesWatch(testing.NewRootWatchAction(sherifvesResource, opts))
}

// Create takes the representation of a sheriff and creates it.  Returns the server's representation of the sheriff, and an error, if there is any.
func (c *FakeSherifves) Create(ctx context.Context, sheriff *v1alpha1.Sheriff, opts v1.CreateOptions) (result *v1alpha1.Sheriff, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootCreateAction(sherifvesResource, sheriff), &v1alpha1.Sheriff{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Sheriff), err
}

// Update takes the representation of a sheriff and updates it. Returns the server's representation of the sheriff, and an error, if there is any.
func (c *FakeSherifves) Update(ctx context.Context, sheriff *v1alpha1.Sheriff, opts v1.UpdateOptions) (result *v1alpha1.Sheriff, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootUpdateAction(sherifvesResource, sheriff), &v1alpha1.Sheriff{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Sheriff), err
}

// UpdateStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating UpdateStatus().
func (c *FakeSherifves) UpdateStatus(ctx context.Context, sheriff *v1alpha1.Sheriff, opts v1.UpdateOptions) (*v1alpha1.Sheriff, error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootUpdateSubresourceAction(sherifvesResource, "status", sheriff), &v1alpha1.Sheriff{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Sheriff), err
}

// Delete takes name of the sheriff and deletes it. Returns an error if one occurs.
func (c *FakeSherifves) Delete(ctx context.Context, name string, opts v1.DeleteOptions) error {
	_, err := c.Fake.
		Invokes(testing.NewRootDeleteActionWithOptions(sherifvesResource, name, opts), &v1alpha1.Sheriff{})
	return err
}

// DeleteCollection deletes a collection of objects.
func (c *FakeSherifves) DeleteCollection(ctx context.Context, opts v1.DeleteOptions, listOpts v1.ListOptions) error {
	action := testing.NewRootDeleteCollectionAction(sherifvesResource, listOpts)

	_, err := c.Fake.Invokes(action, &v1alpha1.SheriffList{})
	return err
}

// Patch applies the patch and returns the patched sheriff.
func (c *FakeSherifves) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts v1.PatchOptions, subresources ...string) (result *v1alpha1.Sheriff, err error) {
	obj, err := c.Fake.
		Invokes(testing.NewRootPatchSubresourceAction(sherifvesResource, name, pt, data, subresources...), &v1alpha1.Sheriff{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Sheriff), err
}

// Apply takes the given apply declarative configuration, applies it and returns the applied sheriff.
func (c *FakeSherifves) Apply(ctx context.Context, sheriff *wildwestv1alpha1.SheriffApplyConfiguration, opts v1.ApplyOptions) (result *v1alpha1.Sheriff, err error) {
	if sheriff == nil {
		return nil, fmt.Errorf("sheriff provided to Apply must not be nil")
	}
	data, err := json.Marshal(sheriff)
	if err != nil {
		return nil, err
	}
	name := sheriff.Name
	if name == nil {
		return nil, fmt.Errorf("sheriff.Name must be provided to Apply")
	}
	obj, err := c.Fake.
		Invokes(testing.NewRootPatchSubresourceAction(sherifvesResource, *name, types.ApplyPatchType, data), &v1alpha1.Sheriff{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Sheriff), err
}

// ApplyStatus was generated because the type contains a Status member.
// Add a +genclient:noStatus comment above the type to avoid generating ApplyStatus().
func (c *FakeSherifves) ApplyStatus(ctx context.Context, sheriff *wildwestv1alpha1.SheriffApplyConfiguration, opts v1.ApplyOptions) (result *v1alpha1.Sheriff, err error) {
	if sheriff == nil {
		return nil, fmt.Errorf("sheriff provided to Apply must not be nil")
	}
	data, err := json.Marshal(sheriff)
	if err != nil {
		return nil, err
	}
	name := sheriff.Name
	if name == nil {
		return nil, fmt.Errorf("sheriff.Name must be provided to Apply")
	}
	obj, err := c.Fake.
		Invokes(testing.NewRootPatchSubresourceAction(sherifvesResource, *name, types.ApplyPatchType, data, "status"), &v1alpha1.Sheriff{})
	if obj == nil {
		return nil, err
	}
	return obj.(*v1alpha1.Sheriff), err
}