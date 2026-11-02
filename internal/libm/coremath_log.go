// The log tables CORE-MATH's asinh, acosh and atanh share, from
// src/binary64/asinh/asinh.c. MIT licensed: see coremath.go.

package libm

var cmLogB = [...]struct {
	c0 uint16
	c1 int16
}{
	{301, 27565}, {7189, 24786}, {13383, 22167}, {18923, 19696}, {23845, 17361}, {28184, 15150},
	{31969, 13054}, {35231, 11064}, {37996, 9173}, {40288, 7372}, {42129, 5657}, {43542, 4020},
	{44546, 2457}, {45160, 962}, {45399, -468}, {45281, -1838}, {44821, -3151}, {44032, -4412},
	{42929, -5622}, {41522, -6786}, {39825, -7905}, {37848, -8982}, {35602, -10020}, {33097, -11020},
	{30341, -11985}, {27345, -12916}, {24115, -13816}, {20661, -14685}, {16989, -15526}, {13107, -16339},
	{9022, -17126}, {4740, -17889},
}

var cmLogR1 = [...]float64{0x1p+0, 0x1.f5076p-1, 0x1.ea4bp-1, 0x1.dfc98p-1, 0x1.d5818p-1,
	0x1.cb72p-1, 0x1.c199cp-1, 0x1.b7f76p-1, 0x1.ae8ap-1,
	0x1.a5504p-1, 0x1.9c492p-1, 0x1.93738p-1, 0x1.8ace6p-1,
	0x1.8258ap-1, 0x1.7a114p-1, 0x1.71f76p-1, 0x1.6a09ep-1,
	0x1.6247ep-1, 0x1.5ab08p-1, 0x1.5342cp-1, 0x1.4bfdap-1,
	0x1.44e08p-1, 0x1.3dea6p-1, 0x1.371a8p-1, 0x1.306fep-1,
	0x1.29e9ep-1, 0x1.2387ap-1, 0x1.1d488p-1, 0x1.172b8p-1,
	0x1.11302p-1, 0x1.0b558p-1, 0x1.059bp-1, 0x1p-1}

var cmLogR2 = [...]float64{0x1p+0, 0x1.ffa74p-1, 0x1.ff4eap-1, 0x1.fef62p-1, 0x1.fe9dap-1,
	0x1.fe452p-1, 0x1.fdeccp-1, 0x1.fd946p-1, 0x1.fd3c2p-1,
	0x1.fce3ep-1, 0x1.fc8bcp-1, 0x1.fc33ap-1, 0x1.fbdbap-1,
	0x1.fb83ap-1, 0x1.fb2bcp-1, 0x1.fad3ep-1, 0x1.fa7c2p-1,
	0x1.fa246p-1, 0x1.f9ccap-1, 0x1.f975p-1, 0x1.f91d8p-1,
	0x1.f8c6p-1, 0x1.f86e8p-1, 0x1.f8172p-1, 0x1.f7bfep-1,
	0x1.f768ap-1, 0x1.f7116p-1, 0x1.f6ba4p-1, 0x1.f6632p-1,
	0x1.f60c2p-1, 0x1.f5b52p-1, 0x1.f55e4p-1, 0x1.f5076p-1}

var cmLogL1 = [...][2]float64{
	{0x0p+0, 0x0p+0}, {-0x1.269e2038315b3p-46, 0x1.62e4eacd4p-6},
	{-0x1.3f2558bddfc47p-45, 0x1.62e3ce7218p-5}, {0x1.07ea13c34efb5p-45, 0x1.0a2ab6d3ecp-4},
	{0x1.8f3e77084d3bap-44, 0x1.62e4a86d8cp-4}, {-0x1.8d92a005f1a7ep-46, 0x1.bb9db7062cp-4},
	{0x1.58239e799bfe5p-44, 0x1.0a2b1a22ccp-3}, {-0x1.a93fcf5f593b7p-44, 0x1.3687f0a298p-3},
	{-0x1.db4cac32fd2b5p-46, 0x1.62e4116b64p-3}, {-0x1.0e65a92ee0f3bp-46, 0x1.8f409e4df6p-3},
	{-0x1.8261383d475f1p-44, 0x1.bb9d15001cp-3}, {-0x1.359886207513bp-44, 0x1.e7f9a8c94p-3},
	{0x1.811f87496ceb7p-44, 0x1.0a2b052ddbp-2}, {0x1.4991ec6cb435cp-44, 0x1.205955ef73p-2},
	{-0x1.4581abfeb8927p-44, 0x1.3687bd9121p-2}, {0x1.cab48f6942703p-44, 0x1.4cb5e8f2b5p-2},
	{-0x1.df2c452fde132p-47, 0x1.62e4420e2p-2}, {0x1.6109f4fdb74bdp-45, 0x1.791292c46ap-2},
	{-0x1.6b95fbdac7696p-44, 0x1.8f40af84e7p-2}, {0x1.7394fa880cbdap-46, 0x1.a56ed8f865p-2},
	{-0x1.50b06a94eccabp-46, 0x1.bb9d6505b4p-2}, {-0x1.be2abf0b38989p-44, 0x1.d1cb91e728p-2},
	{-0x1.7d6bf1e34da04p-44, 0x1.e7f9d139e2p-2}, {-0x1.423c1e14de6edp-44, 0x1.fe27db9b0ep-2},
	{0x1.c46f1a0efbbc2p-44, 0x1.0a2b25060a8p-1}, {0x1.834fe4e3e6018p-45, 0x1.154244482ap-1},
	{0x1.6a03d0f02b65p-46, 0x1.20597312988p-1}, {0x1.d437056526f3p-44, 0x1.2b707145dep-1},
	{-0x1.a0233728405c5p-45, 0x1.3687b0e0b28p-1}, {-0x1.4dbdda10d2bf1p-45, 0x1.419ec5d3f68p-1},
	{0x1.f7d0a25d154f2p-44, 0x1.4cb5f9fc02p-1}, {0x1.15ede4d803b18p-44, 0x1.57cd28421a8p-1},
	{0x1.ef35793c7673p-45, 0x1.62e42fefa38p-1}}

var cmLogL2 = [...][2]float64{
	{0x0p+0, 0x0p+0}, {0x1.5abdac3638e99p-44, 0x1.631ec81ep-11},
	{-0x1.16b8be9bbe239p-45, 0x1.62fd8127p-10}, {-0x1.364c6315542ebp-44, 0x1.0a2520508p-9},
	{0x1.734abe459c9p-45, 0x1.62dadc1dp-9}, {0x1.0cf8a761431bfp-44, 0x1.bb9ff94dp-9},
	{0x1.da2718eb78708p-45, 0x1.0a2a2def8p-8}, {0x1.34ada62c59b93p-44, 0x1.368c0fae4p-8},
	{0x1.d09ab376682d4p-44, 0x1.62e58e4f8p-8}, {-0x1.3cb7b94329211p-45, 0x1.8f46bd28cp-8},
	{-0x1.eec5c297c41dp-45, 0x1.bb9f8312p-8}, {-0x1.6411b9395d15p-44, 0x1.e7fff8f3p-8},
	{-0x1.1c0e59a43053cp-44, 0x1.0a2c0006ep-7}, {0x1.6506596e077b6p-46, 0x1.205bdb6fp-7},
	{0x1.e256bce6faa27p-44, 0x1.36877c86ep-7}, {0x1.bd42467b0c8d1p-51, 0x1.4cb6f5578p-7},
	{-0x1.c4f92132ff0fp-44, 0x1.62e230e8cp-7}, {-0x1.80be08bfab39p-44, 0x1.7911440f6p-7},
	{-0x1.f0b1319ceb1f7p-44, 0x1.8f443020ap-7}, {0x1.a65fcfb8de99bp-45, 0x1.a572dbef4p-7},
	{0x1.4233885d3779cp-46, 0x1.bb9d449a6p-7}, {0x1.f46a59e646edbp-44, 0x1.d1cb8491cp-7},
	{-0x1.c3d2f11c11446p-44, 0x1.e7fd9d2aap-7}, {0x1.7763f78a1e0ccp-45, 0x1.fe2b6f978p-7},
	{0x1.b4c37fc60c043p-44, 0x1.0a2a7c7a5p-6}, {-0x1.5b8a822859be3p-46, 0x1.15412ca86p-6},
	{-0x1.f2d8c9fc064p-44, 0x1.2059c9005p-6}, {-0x1.e80e79c20378dp-44, 0x1.2b703f49bp-6},
	{0x1.68256e4329bdbp-44, 0x1.3688a1a8dp-6}, {0x1.7e9741da248c3p-44, 0x1.419edc7bap-6},
	{0x1.e330dccce602bp-45, 0x1.4cb7034fap-6}, {0x1.2f32b5d18eefbp-49, 0x1.57cd01187p-6},
	{-0x1.269e2038315b3p-46, 0x1.62e4eacd4p-6}}

var cmLogT1 = [...]float64{
	0x1p+0, 0x1.ea4afap-1, 0x1.d5818ep-1, 0x1.c199bep-1, 0x1.ae89f98p-1, 0x1.9c4918p-1,
	0x1.8ace54p-1, 0x1.7a1147p-1, 0x1.6a09e68p-1, 0x1.5ab07ep-1, 0x1.4bfdad8p-1,
	0x1.3dea65p-1, 0x1.306fe08p-1, 0x1.2387a7p-1, 0x1.172b84p-1, 0x1.0b5587p-1, 0x1p-1}

var cmLogT2 = [...]float64{
	0x1p+0, 0x1.fe9d968p-1, 0x1.fd3c228p-1, 0x1.fbdba38p-1, 0x1.fa7c18p-1, 0x1.f91d8p-1,
	0x1.f7bfdbp-1, 0x1.f663278p-1, 0x1.f507658p-1, 0x1.f3ac948p-1, 0x1.f252b38p-1,
	0x1.f0f9c2p-1, 0x1.efa1bfp-1, 0x1.ee4aaap-1, 0x1.ecf483p-1, 0x1.eb9f488p-1}

var cmLogT3 = [...]float64{
	0x1p+0, 0x1.ffe9d2p-1, 0x1.ffd3a58p-1, 0x1.ffbd798p-1, 0x1.ffa74e8p-1, 0x1.ff91248p-1,
	0x1.ff7afb8p-1, 0x1.ff64d38p-1, 0x1.ff4eac8p-1, 0x1.ff38868p-1, 0x1.ff22618p-1,
	0x1.ff0c3dp-1, 0x1.fef61ap-1, 0x1.fedff78p-1, 0x1.fec9d68p-1, 0x1.feb3b6p-1}

var cmLogT4 = [...]float64{
	0x1p+0, 0x1.fffe9dp-1, 0x1.fffd3ap-1, 0x1.fffbd78p-1, 0x1.fffa748p-1, 0x1.fff9118p-1,
	0x1.fff7ae8p-1, 0x1.fff64cp-1, 0x1.fff4e9p-1, 0x1.fff386p-1, 0x1.fff2238p-1,
	0x1.fff0c08p-1, 0x1.ffef5d8p-1, 0x1.ffedfa8p-1, 0x1.ffec98p-1, 0x1.ffeb35p-1}

var cmLogLL = [4][17][3]float64{
	{{0x0p+0, 0x0p+0, 0x0p+0},
		{0x1.62e432b24p-6, -0x1.745af34bb54b8p-42, -0x1.17e3ec05cde7p-97},
		{0x1.62e42e4a8p-5, 0x1.111a4eadf312p-44, 0x1.cff3027abb119p-93},
		{0x1.0a2b233f1p-4, -0x1.88ac4ec78af8p-42, 0x1.4fa087ca75dfdp-93},
		{0x1.62e43056cp-4, 0x1.6bd65e8b0b7p-46, -0x1.b18e160362c24p-95},
		{0x1.bb9d3cbd6p-4, 0x1.de14aa55ec2bp-42, -0x1.c6ac3f1862a6bp-94},
		{0x1.0a2b244dap-3, 0x1.94def487fea7p-42, -0x1.dead1a4581acfp-94},
		{0x1.3687aa9b78p-3, 0x1.9cec9a50db22p-43, 0x1.34a70684f8e0ep-93},
		{0x1.62e42fabap-3, -0x1.d69047a3aebp-44, -0x1.4e061f79144e2p-95},
		{0x1.8f40b56d28p-3, 0x1.de7d755fd2e2p-42, 0x1.bdc7ecf001489p-94},
		{0x1.bb9d3b61fp-3, 0x1.c14f1445b12p-46, 0x1.a1d78cbdc5b58p-93},
		{0x1.e7f9c11f08p-3, -0x1.6e3e0000dae7p-43, 0x1.6a4559fadde98p-94},
		{0x1.0a2b242ec4p-2, 0x1.bb7cf852a5fe8p-42, 0x1.a6aef11ee43bdp-93},
		{0x1.205966c764p-2, 0x1.ad3a5f214294p-45, 0x1.5cc344fa10652p-93},
		{0x1.3687a98aacp-2, 0x1.1623671842fp-45, -0x1.0b428fe1f9e43p-94},
		{0x1.4cb5ec93f4p-2, 0x1.3d50980ea513p-42, 0x1.67f0ea083b1c4p-93},
		{0x1.62e42fefa4p-2, -0x1.8432a1b0e264p-44, 0x1.803f2f6af40f3p-93},
	},
	{{0x0p+0, 0x0p+0, 0x0p+0},
		{0x1.62e462b4p-10, 0x1.061d003b97318p-42, 0x1.d7faee66a2e1ep-93},
		{0x1.62e44c92p-9, 0x1.95a7bff5e239p-42, -0x1.f7e788a87135p-95},
		{0x1.0a2b1e33p-8, 0x1.2a3a1a65aa3ap-43, -0x1.54599c9605442p-93},
		{0x1.62e4367cp-8, -0x1.4a995b6d9ddcp-45, -0x1.56bb79b254f33p-100},
		{0x1.bb9d449ap-8, 0x1.8a119c42e9bcp-42, -0x1.8ecf7d8d661f1p-93},
		{0x1.0a2b1f19p-7, 0x1.8863771bd10a8p-42, 0x1.e9731de7f0155p-94},
		{0x1.3687ad11p-7, 0x1.e026a347ca1c8p-42, 0x1.fadc62522444dp-97},
		{0x1.62e436f28p-7, 0x1.25b84f71b70b8p-42, -0x1.fcb3f98612d27p-96},
		{0x1.8f40b7b38p-7, -0x1.62a0a4fd4758p-43, 0x1.3cb3c35d9f6a1p-93},
		{0x1.bb9d3abbp-7, -0x1.0ec48f94d786p-42, -0x1.6b47d410e4cc7p-93},
		{0x1.e7f9bb23p-7, 0x1.e4415cbc97ap-43, -0x1.3729fdb677231p-93},
		{0x1.0a2b22478p-6, -0x1.cb73f4505b03p-42, -0x1.1b3b3a3bc370ap-93},
		{0x1.2059691e8p-6, -0x1.abcc3412f264p-43, -0x1.fe6e998e48673p-95},
		{0x1.3687a768p-6, -0x1.43901e5c97a9p-42, 0x1.b54cdd52a5d88p-96},
		{0x1.4cb5eb5d8p-6, -0x1.8f106f00f13b8p-42, -0x1.8f793f5fce148p-93},
		{0x1.62e432b24p-6, -0x1.745af34bb54b8p-42, -0x1.17e3ec05cde7p-97},
	},
	{{0x0p+0, 0x0p+0, 0x0p+0},
		{0x1.62e7bp-14, -0x1.868625640a68p-44, -0x1.34bf0db910f65p-93},
		{0x1.62e35f6p-13, -0x1.2ee3d96b696ap-43, 0x1.a2948cd558655p-94},
		{0x1.0a2b4b2p-12, 0x1.53edbcf1165p-47, -0x1.cfc26ccf6d0e4p-97},
		{0x1.62e4be1p-12, 0x1.783e334614p-52, -0x1.04b96da30e63ap-93},
		{0x1.bb9e085p-12, -0x1.60785f20acb2p-43, -0x1.f33369bf7dff1p-96},
		{0x1.0a2b94dp-11, 0x1.fd4b3a273353p-42, -0x1.685a35575eff1p-96},
		{0x1.368810f8p-11, 0x1.7ded26dc813p-47, -0x1.4c4d1abca79bfp-96},
		{0x1.62e47878p-11, 0x1.7d2bee9a1f63p-42, 0x1.860233b7ad13p-93},
		{0x1.8f40cb48p-11, -0x1.af034eaf471cp-42, 0x1.ae748822d57b7p-94},
		{0x1.bb9d094p-11, -0x1.7a223013a20fp-42, -0x1.1e499087075b6p-93},
		{0x1.e7fa32c8p-11, -0x1.b2e67b1b59bdp-43, -0x1.54a41eda30fa6p-93},
		{0x1.0a2b237p-10, -0x1.7ad97ff4ac7ap-44, 0x1.f932da91371ddp-93},
		{0x1.2059a338p-10, -0x1.96422d90df4p-44, -0x1.90800fbbf2ed3p-94},
		{0x1.36879824p-10, 0x1.0f9054001812p-44, 0x1.9567e01e48f9ap-93},
		{0x1.4cb602cp-10, -0x1.0d709a5ec0b5p-43, 0x1.253dfd44635d2p-94},
		{0x1.62e462b4p-10, 0x1.061d003b97318p-42, 0x1.d7faee66a2e1ep-93},
	},
	{{0x0p+0, 0x0p+0, 0x0p+0},
		{0x1.63007cp-18, -0x1.db0e38e5aaaap-43, 0x1.259a7b94815b9p-93},
		{0x1.6300f6p-17, 0x1.2b1c75580438p-44, 0x1.78cabba01e3e4p-93},
		{0x1.0a2115p-16, -0x1.5ff223730759p-42, 0x1.8074feacfe49dp-95},
		{0x1.62e1ecp-16, -0x1.85d6f6487ce4p-45, 0x1.05485074b9276p-93},
		{0x1.bba301p-16, -0x1.af5d58a7c921p-43, -0x1.30a8c0fd2ff5fp-93},
		{0x1.0a32298p-15, 0x1.590faa0883bdp-43, 0x1.95e9bda999947p-93},
		{0x1.3682f1p-15, 0x1.f0224376efaf8p-42, -0x1.5843c0db50d1p-93},
		{0x1.62e3d8p-15, -0x1.142c13daed4ap-43, 0x1.c68a61183ce87p-93},
		{0x1.8f44dd8p-15, -0x1.aa489f399931p-43, 0x1.11c5c376854eap-94},
		{0x1.bb9601p-15, 0x1.9904d8b6a3638p-42, 0x1.8c89554493c8fp-93},
		{0x1.e7f744p-15, 0x1.5785ddbe7cba8p-42, 0x1.e7ff3cde7d70cp-94},
		{0x1.0a2c53p-14, -0x1.6d9e8780d0d5p-43, 0x1.ad9c178106693p-94},
		{0x1.205d134p-14, -0x1.214a2e893fccp-43, 0x1.548a9500c9822p-93},
		{0x1.3685e28p-14, 0x1.e23588646103p-43, 0x1.2a97b26da2d88p-94},
		{0x1.4cb6c18p-14, 0x1.2b7cfcea9e0d8p-42, -0x1.5095048a6b824p-93},
		{0x1.62e7bp-14, -0x1.868625640a68p-44, -0x1.34bf0db910f65p-93},
	},
}

var cmLogCH = [...][2]float64{
	{0x1p-1, 0x1.24b67ee516e3bp-111}, {-0x1p-2, -0x1.932ce43199a8dp-110}, {0x1.5555555555555p-3, 0x1.55540c15cf91fp-57},
}
