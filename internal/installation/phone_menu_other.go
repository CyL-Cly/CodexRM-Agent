//go:build !windows

package installation

func RegisterPhoneMenu(string) error   { return nil }
func unregisterPhoneMenu(string) error { return nil }
func ChoosePhoneFileTarget(_ string, names []string) (int, error) {
	if len(names) == 1 {
		return 0, nil
	}
	return -1, nil
}
func ShowPhoneQueued(string, string) {}
