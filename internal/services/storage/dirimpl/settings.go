package dirimpl

// Settings configures a Service. RootDir is where buckets are stored.
type Settings struct {
	RootDir string `validate:"required"`
}
