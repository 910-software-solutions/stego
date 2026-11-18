package declaration

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jsell-rh/stego/internal/types"
)

// Scaffold makes the declaration of a new service of the named archetype (ST-DECL-CMD01). The caller gives the facts of
// the archetype that it resolved from the registry: its language, and the declaration text of its example content
// (ST-SUP-EN03 field Example content) with its placeholders. Scaffold puts the service name and the module path in the
// placeholders, and refuses a name that is not a service name and a text that does not declare a service of the
// archetype and language. With no example text, the declaration holds no example content: no entity and no collection.
func Scaffold(name, archetype, language string, example []byte, module, servicePlaceholder, modulePlaceholder string) ([]byte, error) {
	// The service takes the name of the project folder. Init refuses a name that checking would refuse, before the
	// name goes into any file (ST-DECL-INV86).
	if !serviceNameForm.MatchString(name) {
		return nil, fmt.Errorf("the project folder name %q is not a service name: a service name is 1 to 63 lower-case letters, digits and hyphens, starts with a letter and does not end with a hyphen, because the Kubernetes Service and the database roles take it (ST-DECL-Q43); rename the folder and run stego init again", name)
	}
	if len(example) == 0 {
		return yaml.Marshal(types.ServiceDeclaration{Kind: "service", Name: name, Archetype: archetype, Language: language, Entities: []types.Entity{}, Collections: []types.Collection{}})
	}
	text := []byte(strings.NewReplacer(servicePlaceholder, name, modulePlaceholder, module).Replace(string(example)))
	var head struct {
		Kind      string `yaml:"kind"`
		Name      string `yaml:"name"`
		Archetype string `yaml:"archetype"`
		Language  string `yaml:"language"`
	}
	if err := yaml.Unmarshal(text, &head); err != nil {
		return nil, fmt.Errorf("the example declaration of archetype %s: %w", archetype, err)
	}
	if head.Kind != "service" || head.Name != name || head.Archetype != archetype || head.Language != language {
		return nil, fmt.Errorf("the example declaration of archetype %s must declare kind service, name %s, archetype %s and language %s", archetype, servicePlaceholder, archetype, language)
	}
	return text, nil
}
