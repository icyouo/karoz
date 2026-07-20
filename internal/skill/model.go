package skill

type Skill struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	ShortDescription string `json:"short_description,omitempty"`
	Path             string `json:"path"`
	Scope            string `json:"scope"`
}
