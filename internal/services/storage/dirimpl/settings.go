package dirimpl

// Settings configures a Service. RootDir is where buckets are stored. Buckets
// names buckets the app creates at startup if they do not exist yet.
//
// QueuesEndpoint and FunctionsEndpoint are not the storage's: they are the
// base URLs of the queues and functions services, whose service-send and
// service-invoke endpoints bucket event notifications are validated and
// sent through, or empty to have no queue or function destination. They
// are settings of this section, TULE_Storage_QueuesEndpoint and
// TULE_Storage_FunctionsEndpoint, as every Tule service puts the endpoints
// of the services it calls in its own section
// (TULE_Store_FunctionsEndpoint, TULE_Functions_QueuesEndpoint); the app
// reads them, and the storage never does.
type Settings struct {
	RootDir           string `validate:"required"`
	QueuesEndpoint    string `validate:"omitempty,http_url"`
	FunctionsEndpoint string `validate:"omitempty,http_url"`
	Buckets           []string
}
