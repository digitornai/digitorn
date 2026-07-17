package contextsvc

import "math"

const (
	calibClampLo = 0.25
	calibClampHi = 4.0
)

func CalibrateTotal(raw, providerAnchor int, oldRatio float64) (total int, ratio float64) {
	ratio = oldRatio
	if providerAnchor > 0 && raw > 0 {
		inst := float64(providerAnchor) / float64(raw)
		if inst < calibClampLo {
			inst = calibClampLo
		} else if inst > calibClampHi {
			inst = calibClampHi
		}
		if ratio <= 0 {
			ratio = inst
		} else {
			ratio = 0.5*ratio + 0.5*inst
		}
		return providerAnchor, ratio
	}
	if ratio <= 0 {
		ratio = 1.0
	}
	if raw <= 0 {
		return 0, ratio
	}
	return int(math.Round(float64(raw) * ratio)), ratio
}
