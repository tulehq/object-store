package dirimpl

// Settings configures a Service. RootDir is where buckets are stored. Buckets
// names buckets the app creates at startup if they do not exist yet.
type Settings struct {
	RootDir string `validate:"required"`
	Buckets []string
}
