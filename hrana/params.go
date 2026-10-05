package hrana

import "github.com/samyfodil/musql/engine"

// paramNames is a statement's parameters by index, 1-based in SQL and
// 0-based here: the name with its sigil, or "" for an anonymous "?".
func paramNames(q string) ([]string, error) {
	info, err := engine.ParseParamInfo(q)
	if err != nil {
		return nil, err
	}
	names := make([]string, info.NumParams)
	for name, idx := range info.ParamNames {
		if idx >= 1 && idx <= len(names) {
			names[idx-1] = name
		}
	}
	return names, nil
}
