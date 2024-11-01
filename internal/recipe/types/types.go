/*
 * Copyright 2024 Damian Peckett <damian@pecke.tt>.
 *
 * Licensed under the Immutos Community Edition License, Version 1.0
 * (the "License"); you may not use this file except in compliance with
 * the License. You may obtain a copy of the License at
 *
 *    http://immutos.com/licenses/LICENSE-1.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package types

type TypeMeta struct {
	// APIVersion is the version of the API.
	APIVersion string `yaml:"apiVersion" mapstructure:"apiVersion"`
	// Kind is the kind of the resource.
	Kind string `yaml:"kind" mapstructure:"kind"`
}

type Typed interface {
	GetAPIVersion() string
	GetKind() string
	PopulateTypeMeta()
}
