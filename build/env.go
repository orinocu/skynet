package build

var (
	// mongoDBHost is the environment variable that tells
	mongoDBURI = "MONGODB_URI"

	// siaAPIPassword is the environment variable that sets a custom API
	// password if the default is not used
	siaAPIPassword = "SIA_API_PASSWORD"

	// siaDataDir is the environment variable that tells siad where to put the
	// general sia data, e.g. api password, configuration, logs, etc.
	siaDataDir = "SIA_DATA_DIR"

	// siadDataDir is the environment variable which tells siad where to put the
	// siad-specific data
	siadDataDir = "SIAD_DATA_DIR"

	// siaWalletPassword is the environment variable that can be set to enable
	// auto unlocking the wallet
	siaWalletPassword = "SIA_WALLET_PASSWORD"

	// siaExchangeRate is the environment variable that can be set to
	// show amounts (additionally) in a different currency
	siaExchangeRate = "SIA_EXCHANGE_RATE"

	// tusMaxSize determines the max size of an upload via the /tus endpoint.
	tusMaxSize = "TUS_MAXSIZE"
)
