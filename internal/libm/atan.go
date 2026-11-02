// Ported from CORE-MATH, src/binary64/atan/atan.c, for glibc's atan, which
// is IBM's. MIT licensed: see coremath.go.

package libm

import "math"

// Atan is atan, correctly rounded: glibc's except where glibc is not.
func Atan(x float64) float64 {
	t := asuint64(x)
	at := t & (^uint64(0) >> 1)
	i := int64(at>>51) - 2030
	if at < 0x3f7b21c475e6362a {
		// |x| < 0x1.b21c475e6362ap-8.
		switch {
		case at == 0:
			return x
		case at < 0x3e40000000000000:
			// |x| < 0x1p-27.
			return math.FMA(-0x1p-54, x, x)
		}
		x2 := x * x
		x3, x4 := x*x2, x2*x2
		c := &atanSmallCH
		f := float64(x3 * ((c[0] + float64(x2*c[1])) + float64(x4*(c[2]+float64(x2*c[3])))))
		epsp, epsm := float64(f*0x1.6p-50), float64(f*0x1.6p-51)
		ub, lb := (f+epsp)+x, (f-epsm)+x
		if ub == lb {
			return ub
		}
		return atanRefine2(x, ub)
	}
	var h, ah, al float64
	if at > 0x4062ded8e34a9035 {
		// |x| > 0x1.2ded8e34a9035p+7.
		ah = math.Copysign(0x1.921fb54442d18p+0, x)
		al = math.Copysign(0x1.1a62633145c07p-54, x)
		if at >= 0x434d02967c31cdb5 {
			// |x| >= 0x1.d02967c31cdb5p+53.
			if at > 0x7ff<<52 {
				return x + x
			}
			return ah + al
		}
		h = -1.0 / x
	} else {
		// 1 <= i <= 30.
		u := t & (^uint64(0) >> 13)
		ut := u >> (51 - 16)
		ut2 := ut * ut >> 16
		c := &atanC[i]
		i = int64((uint64(c[0])<<16 + ut*uint64(c[1]) - ut2*uint64(c[2])) >> (16 + 9))
		sgn := math.Copysign(1.0, x)
		ta, id := float64(sgn*atanA[i][0]), sgn*float64(i)
		al = float64(sgn*atanA[i][1]) + float64(0x1.8469898cc517p-55*id)
		h = (x - ta) / (1 + float64(x*ta))
		ah = float64(0x1.921fb54442dp-7 * id)
	}
	h2 := h * h
	h4 := h2 * h2
	f := (1 + float64(h2*-0x1.555555555552bp-2)) + float64(h4*(0x1.9999999069c2p-3+float64(h2*-0x1.248d2c8444ac6p-3)))
	al = math.FMA(h, f, al)
	e := float64(h * 0x3.fp-52)
	ub, lb := (al+e)+ah, (al-e)+ah
	if ub == lb {
		return ub
	}
	return atanRefine2(x, ub)
}

// atanRefine2 is atan(x) for 0x1p-27 <= |x|, where a is the fast path's
// approximation of it.
func atanRefine2(x, a float64) float64 {
	phi := asuint64(float64(math.Abs(a)*0x1.45f306dc9c883p6) + 256.5)
	i := int64(phi>>(52-8)) & 0xff
	var h, hl float64
	if i == 128 {
		h = -1.0 / x
		hl = float64(math.FMA(h, x, 1) * h)
	} else {
		ta := math.Copysign(atanA[i][0], x)
		zta := float64(x * ta)
		ztal := math.FMA(x, ta, -zta)
		zmta := x - ta
		v := 1 + zta
		d := 1 - v
		ev := (d + zta) - ((d + v) - 1) + ztal
		r := 1.0 / v
		rl := (math.FMA(r, -v, 1.0) - float64(ev*r)) * r
		h = float64(r * zmta)
		hl = math.FMA(r, zmta, -h) + float64(rl*zmta)
	}
	h2, h2l := acoshMulDDAcc(h, hl, h, hl)
	h4 := h2 * h2
	h3, h3l := acoshMulDDAcc(h, hl, h2, h2l)
	cl := &atanRefCL
	fl := float64(h2 * ((cl[0] + float64(h2*cl[1])) + float64(h4*(cl[2]+float64(h2*cl[3])))))
	// atan's polydd normalizes its products with fastTwoSum rather than
	// asinh's two lines, which is the same in round-to-nearest.
	f, fl := asinhPolyDD(h2, h2l, atanRefCH[:], fl)
	f, fl = acoshMulDDAcc(h3, h3l, f, fl)
	var ah, al, at float64
	if i == 0 {
		ah, al, at = h, f, fl
	} else {
		var df float64
		if i < 128 {
			df = float64(math.Copysign(1.0, x) * atanA[i][1])
		}
		id := math.Copysign(float64(i), x)
		ah = float64(0x1.921fb54442dp-7 * id)
		al = float64(0x1.8469898cc518p-55 * id)
		at = float64(-0x1.fc8f8cbb5bf8p-104 * id)
		al, at = asinhAddDD(al, at, df, 0)
		al, at = asinhAddDD(al, at, h, hl)
		al, at = asinhAddDD(al, at, f, fl)
	}
	v0, v2 := fastTwoSum(ah, al)
	v1, v2 := fastTwoSum(v2, at)
	t0, t1 := asuint64(v0), asuint64(v1)
	if (t1+1)&(^uint64(0)>>12) <= 2 || (t0>>52)&0x7ff-(t1>>52)&0x7ff > 103 {
		ax := math.Abs(x)
		for _, d := range atanDB {
			if ax == d[0] {
				return math.Copysign(d[1], x) + float64(math.Copysign(1.0, x)*d[2])
			}
		}
		if t1&(^uint64(0)>>12) == 0 {
			if (asuint64(v2)^t1)>>63 != 0 {
				t1--
			} else {
				t1++
			}
			v1 = asdouble(t1)
		}
	}
	return v1 + v0
}

var atanA = [...][2]float64{
	{0x0p+0, 0x0p+0}, {0x1.9224e047e368ep-7, 0x1.a3ca6c727c59dp-62},
	{0x1.92346247a91fp-6, 0x1.138b0ef96a186p-64}, {0x1.2dbaae9a05dbp-5, 0x1.36e7f8a3f5e42p-59},
	{0x1.927278a3b1162p-5, -0x1.ac986efb92662p-64}, {0x1.f7495ea3f3783p-5, 0x1.06ec8011ee816p-59},
	{0x1.2e239ccff3831p-4, -0x1.858437d431332p-58}, {0x1.60b9f7597fdecp-4, -0x1.cebd13eb7c513p-60},
	{0x1.936bb8c5b2da2p-4, -0x1.840cac0d81db5p-58}, {0x1.c63ce377fc802p-4, 0x1.400b0fdaa109ep-58},
	{0x1.f93183a8db9e9p-4, 0x1.0e04e06c86e72p-59}, {0x1.1626d85a91e7p-3, 0x1.f7ad829163ca7p-59},
	{0x1.2fcac73a6064p-3, -0x1.2680735ce2cd8p-58}, {0x1.4986a74cf4e57p-3, -0x1.90559690b42e4p-57},
	{0x1.635c990ce0d36p-3, 0x1.91d29110b41aap-58}, {0x1.7d4ec54fb5968p-3, -0x1.ea90e2718278p-59},
	{0x1.975f5e0553158p-3, -0x1.dc82ac14e3e1cp-61}, {0x1.b1909efd8b762p-3, -0x1.73a10fd13daafp-58},
	{0x1.cbe4ceb4b4cf2p-3, -0x1.3a7ffbeabda0bp-57}, {0x1.e65e3f27c9f2ap-3, -0x1.db6627a24d523p-57},
	{0x1.007fa758626aep-2, -0x1.45f97dd3099f6p-57}, {0x1.0de53475f3b3cp-2, -0x1.6293f68741816p-57},
	{0x1.1b6103d3597e9p-2, -0x1.ab240d40633e9p-57}, {0x1.28f459ecad74dp-2, -0x1.de34d14e832ep-61},
	{0x1.36a08355c63dcp-2, 0x1.af540d9fb4926p-57}, {0x1.4466d542bac92p-2, 0x1.da60fdbc82ac4p-57},
	{0x1.5248ae1701b17p-2, -0x1.92a601170138ap-56}, {0x1.604775fbb27dfp-2, -0x1.7f1fca1d5d15bp-57},
	{0x1.6e649f7d78649p-2, -0x1.4e223ea716c7bp-57}, {0x1.7ca1a832d0f84p-2, 0x1.b24c824ac51fcp-56},
	{0x1.8b00196b3d022p-2, 0x1.4314cd132ba43p-57}, {0x1.998188e816bfp-2, -0x1.11f1e0817879ap-56},
	{0x1.a827999fcef32p-2, -0x1.c3dea4dbad538p-57}, {0x1.b6f3fc8c61e5bp-2, 0x1.60d1b780ee3ebp-57},
	{0x1.c5e87185e67b6p-2, -0x1.ab5edb7dfa545p-59}, {0x1.d506c82a2c8p-2, -0x1.8e1437048b5bdp-57},
	{0x1.e450e0d273e7ap-2, -0x1.06951c97b050fp-56}, {0x1.f3c8ad985d9eep-2, -0x1.14af9522ab518p-59},
	{0x1.01b819b5a7cf7p-1, -0x1.aba0d7d97d1f2p-56}, {0x1.09a4c59bd0d4dp-1, 0x1.095bc4ebc2c42p-59},
	{0x1.11ab7190834ecp-1, 0x1.798826fa27774p-55}, {0x1.19cd3fe8e405dp-1, 0x1.008f6258fc98fp-55},
	{0x1.220b5ef047825p-1, -0x1.462af7ceb7de6p-58}, {0x1.2a6709a74f289p-1, -0x1.1184dfd78b472p-56},
	{0x1.32e1889047ffdp-1, 0x1.9141876dc40c5p-56}, {0x1.3b7c3289ed6f3p-1, 0x1.481c20189726cp-55},
	{0x1.44386db9ce5dbp-1, 0x1.2e851bd025441p-55}, {0x1.4d17b087b265dp-1, 0x1.13ada9b8bc419p-56},
	{0x1.561b82ab7f99p-1, -0x1.05b4c3c4cbee8p-55}, {0x1.5f457e4f4812ep-1, -0x1.5619249bd96f1p-55},
	{0x1.6897514751db6p-1, -0x1.b0a0fbcafc671p-57}, {0x1.7212be621be6dp-1, -0x1.19ff2dc66da45p-55},
	{0x1.7bb99ed2990cfp-1, 0x1.1320449592d92p-55}, {0x1.858de3b716571p-1, -0x1.1fddcd2f3da8ep-55},
	{0x1.8f9197bf85eebp-1, 0x1.d44a42e35cc97p-57}, {0x1.99c6e0f634394p-1, -0x1.585a178b4a18dp-56},
	{0x1.a43002ae4285p-1, 0x1.f95a531b3a97p-57}, {0x1.aecf5f9ba35a6p-1, -0x1.96c2d43ca3392p-60},
	{0x1.b9a77c18c1af2p-1, -0x1.a5bed94b05defp-57}, {0x1.c4bb009e77983p-1, 0x1.54509d2bff511p-59},
	{0x1.d00cbc7384d2ep-1, -0x1.b4c867cef300cp-57}, {0x1.db9fa89953fcfp-1, -0x1.ddfac663d6bc6p-62},
	{0x1.e776eafc91706p-1, -0x1.a510683ff7cb6p-56}, {0x1.f395d9f0e3c92p-1, 0x1.4fdcd8e4e871p-59},
	{0x1p+0, 0x0p+0}, {0x1.065c900aaf2d8p+0, -0x1.deec7fc9042adp-55},
	{0x1.0ce29d0883c99p+0, -0x1.395ae45e0657dp-55}, {0x1.139447e6a86eep+0, 0x1.332cf301a97f3p-55},
	{0x1.1a73d55278c4bp+0, -0x1.6cc8c4b78213bp-55}, {0x1.2183b0c4573ffp+0, 0x1.70a90841da57ap-55},
	{0x1.28c66fdaf8f09p+0, -0x1.ba39bad450eep-57}, {0x1.303ed61109e2p+0, -0x1.8692946d9f93cp-55},
	{0x1.37efd8d87607ep+0, 0x1.3b711bf765b58p-57}, {0x1.3fdca42847507p+0, 0x1.c21387985b081p-56},
	{0x1.48089f8bf42ccp+0, -0x1.7ddb19d3d0efcp-55}, {0x1.507773c537eadp+0, -0x1.f5e354cf971f3p-56},
	{0x1.592d11142fa55p+0, -0x1.00f0ad675330dp-56}, {0x1.622db63c8ecc2p+0, -0x1.2c93f50ab2c0ep-55},
	{0x1.6b7df862652p+0, 0x1.bec391adc37d5p-56}, {0x1.7522cbdd428a8p+0, -0x1.9686ddc9ffcf5p-57},
	{0x1.7f218e25a7461p+0, -0x1.8d16529514246p-56}, {0x1.89801106cc709p+0, -0x1.092f51e9c2803p-55},
	{0x1.9444a7462122ap+0, -0x1.07c06755404c4p-55}, {0x1.9f7632fa9e871p+0, 0x1.02e0d43abc92bp-55},
	{0x1.ab1c35d8a74eap+0, 0x1.d0184e48af6f7p-58}, {0x1.b73ee3c3ef16ap+0, 0x1.73be957380bc2p-56},
	{0x1.c3e738086bc0fp+0, -0x1.02b6e26c84462p-56}, {0x1.d11f0dae40609p+0, 0x1.25c4f3ffa6e1fp-58},
	{0x1.def13b73c1406p+0, -0x1.e302db3c6823fp-58}, {0x1.ed69b4153a45dp+0, 0x1.3207830326c0ep-56},
	{0x1.fc95abad6cf4ap+0, -0x1.6308cee7927bfp-57}, {0x1.0641e192ceab3p+1, -0x1.0147ebf0df4c5p-56},
	{0x1.0ea21d716fbf7p+1, -0x1.168533cc41d8bp-56}, {0x1.17749711a6679p+1, -0x1.52a0b0333e9c5p-57},
	{0x1.20c36c6a7f38ep+1, 0x1.8659eece35395p-57}, {0x1.2a99f50fd4f4fp+1, 0x1.20fcad18cb36fp-55},
	{0x1.3504f333f9de6p+1, -0x1.52afdbd5a8c74p-56}, {0x1.4012ce2586a17p+1, -0x1.9747a792907d7p-56},
	{0x1.4bd3d87fe065p+1, 0x1.90c59393b52c8p-56}, {0x1.585aa4e1530fap+1, 0x1.af6934f13a3a8p-56},
	{0x1.65bc6cc825147p+1, -0x1.8534dcab5ad3ep-59}, {0x1.74118e4b6a7c8p+1, -0x1.555aa8bfca9a1p-56},
	{0x1.837626d70fdb8p+1, -0x1.56b3fee9ca72bp-58}, {0x1.940ad30abc792p+1, 0x1.4b3fdd4fdc06cp-58},
	{0x1.a5f59e90600ddp+1, 0x1.285d367c55ddcp-57}, {0x1.b9633283b6d14p+1, -0x1.8712976f17a16p-59},
	{0x1.ce885653127e7p+1, -0x1.abe8ab65d49fcp-60}, {0x1.e5a3de972a377p+1, 0x1.cd9be81ad764bp-58},
	{0x1.ff01305ecd8dcp+1, 0x1.742c2922656fap-59}, {0x1.0d7dc7cff4c9ep+2, -0x1.7c842978bee09p-56},
	{0x1.1d0143e71565fp+2, 0x1.7bc7dea7c3c03p-57}, {0x1.2e4ff1626b949p+2, 0x1.aefbe25b404e9p-59},
	{0x1.41bfee2424771p+2, -0x1.4bcfaaa95cb2cp-60}, {0x1.57be4eaa5e11bp+2, 0x1.0fe741e4ec679p-58},
	{0x1.70d751908c1b1p+2, 0x1.fe74a5b0ec709p-58}, {0x1.8dc25c117782bp+2, 0x1.0ca1c19f710efp-58},
	{0x1.af73f4ca3310fp+2, 0x1.2867b40ba77d6p-58}, {0x1.d7398d15e70dbp+2, 0x1.0fd4e0d4b1547p-57},
	{0x1.0372fb36b87e2p+3, 0x1.c16c9ecc1621dp-58}, {0x1.208dbdae055efp+3, 0x1.6b81a36e75e8cp-58},
	{0x1.44e6c595afdccp+3, -0x1.7c22045771848p-58}, {0x1.7398c57f3f1adp+3, 0x1.970503be105cp-58},
	{0x1.b1d03c03d2f7fp+3, -0x1.f299d010aead2p-60}, {0x1.046e9fe60a77ep+4, 0x1.d2b61deff33ecp-58},
	{0x1.45affed201b55p+4, 0x1.0e84d9567203ap-64}, {0x1.b267195b1ffaep+4, -0x1.ad44b44b92653p-64},
	{0x1.45e2455e4aaa7p+5, -0x1.296d577b5e21dp-60}, {0x1.45eed6854ce99p+6, 0x1.2db53886013cap-63},
	{0, 0},
}

var atanC = [31][3]uint16{
	{419, 81, 0}, {500, 81, 0}, {582, 163, 0}, {745, 163, 0}, {908, 326, 0}, {1234, 326, 0},
	{1559, 651, 0}, {2210, 650, 1}, {2860, 1299, 3}, {4156, 1293, 4}, {5444, 2569, 24},
	{7989, 2520, 32}, {10476, 4917, 168}, {15224, 4576, 200}, {19601, 8341, 838},
	{27105, 6648, 731}, {33036, 10210, 1998}, {41266, 6292, 1117}, {46469, 7926, 2048},
	{52375, 4038, 849}, {55587, 4591, 1291}, {58906, 2172, 479}, {60612, 2390, 688},
	{62325, 1107, 247}, {63192, 1207, 349}, {64056, 556, 124}, {64491, 605, 175},
	{64923, 278, 62}, {65141, 303, 88}, {65358, 139, 31}, {65467, 151, 44},
}

var atanRefCH = [...][2]float64{
	{-0x1.5555555555555p-2, -0x1.5555555555555p-56}, {0x1.999999999999ap-3, -0x1.999999999bcb8p-57},
	{-0x1.2492492492492p-3, -0x1.249242093c016p-57}}

var atanRefCL = [...]float64{
	0x1.c71c71c71c71cp-4, -0x1.745d1745d1265p-4, 0x1.3b13b115bcbc4p-4, -0x1.1107c41ad3253p-4}

var atanDB = [...][3]float64{
	{0x1.0dc89a3b5501p-7, 0x1.0dc70ac228717p-7, 0x1p-61},
	{0x1.e3fb41d2d226p-8, 0x1.e3f9013a852f8p-8, 0x1p-62},
	{0x1.7ba49f739829fp-1, 0x1.46ac372243536p-1, 0x1p-109},
	{0x1.a933fe176b375p-3, 0x1.a33f32ac5ceb5p-3, -0x1p-112},
	{0x1.bb04a79820063p-8, 0x1.bb02ed5c5e956p-8, -0x1p-115},
	{0x1.cd30a9499618bp-8, 0x1.cd2eb65f92a46p-8, -0x1p-112},
	{0x1.f44aa37b8e66bp-7, 0x1.f440b04187c87p-7, -0x1p-112},
	{0x1.fd2ac95e57ef9p-8, 0x1.fd2829febc03ap-8, -0x1p-112},
	{0x1.6419079bbf601p-6, 0x1.640aade8f5427p-6, 0x1p-114},
	{0x1.7ba49f739829fp-1, 0x1.46ac372243536p-1, 0x1p-110},
	{0x1.d768804487b07p-3, 0x1.cf5676f373ec1p-3, -0x1p-110},
	{0x1.bb04a79820063p-8, 0x1.bb02ed5c5e956p-8, -0x1p-115},
}

var atanSmallCH = [...]float64{
	-0x1.5555555555555p-2, 0x1.99999999998c1p-3, -0x1.249249176aecp-3, 0x1.c711fd121ae8p-4}
