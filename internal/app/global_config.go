package app

import internalconfig "github.com/bucketgit/bgit/internal/config"

const globalConfigVersion = internalconfig.GlobalVersion

func defaultGlobalConfigPath() (string, error) { return internalconfig.DefaultGlobalPath() }
func defaultLocalBrokerRoot() (string, error)  { return internalconfig.DefaultLocalBrokerRoot() }
func defaultBGitCacheDir() (string, error)     { return internalconfig.DefaultCacheDir() }
func readGlobalConfig(path string) (internalconfig.Global, error) {
	return internalconfig.ReadGlobal(path)
}
func writeGlobalConfig(path string, value internalconfig.Global) error {
	return internalconfig.WriteGlobal(path, value)
}
