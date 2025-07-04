package go_frodokem

import (
	"crypto/aes"
	"encoding/binary"
	"errors"
	"golang.org/x/crypto/sha3"
	"math"
)

// Generate a key-pair
func (k *FrodoKEM) Keygen() (pk []uint8, sk []uint8) {

	// Step 1: Allocate and fill seed buffer (s || seedSE || z) with cryptographically secure random data
	sSeedSEz := make([]byte, k.lenS/8+k.lenSeedSE/8+k.lenZ/8)
	k.rng(sSeedSEz)

	// Extract secret value 's' used for secret key
	s := sSeedSEz[0 : k.lenS/8]

	// Extract seed used to derive secret matrices S and E
	seedSE := sSeedSEz[k.lenS/8 : k.lenS/8+k.lenSeedSE/8]

	// Extract randomness used to generate matrix A deterministically
	z := sSeedSEz[k.lenS/8+k.lenSeedSE/8 : k.lenS/8+k.lenSeedSE/8+k.lenZ/8]

	// Generate seedA by hashing z — seedA is used to create matrix A deterministically
	seedA := k.shake(z, k.lenSeedA/8)

	// Generate matrix A from seedA (public matrix)
	A := k.gen(seedA)

	// Generate pseudorandom bitstring used for sampling S and E matrices
	r := unpackUint16(k.shake(append([]byte{0x5f}, seedSE...), 2*k.n*k.nBar*k.lenChi/8))
	// Sample Sᵗ (transposed secret matrix) from noise distribution
	Stransposed := k.sampleMatrix(r[0:k.n*k.nBar], k.nBar, k.n)

	// Transpose Sᵗ to obtain S
	S := matrixTranspose(Stransposed)

	// Sample error matrix E from noise distribution
	E := k.sampleMatrix(r[k.n*k.nBar:2*k.n*k.nBar], k.n, k.nBar)

	// Compute matrix B = AS + E mod q (LWE public component)
	B := matrixAddWithMod(matrixMulWithMod(A, S, k.q), E, k.q)

	// Pack matrix B into byte string
	b := k.pack(B)

	// Public key = seedA || packed B
	pk = append(seedA, b...)

	// Compute hash of public key (used later for KDF in shared secret derivation)
	pkh := k.shake(pk, k.lenPkh/8)

	//encode the transposed matrix row by row
	stb := make([]uint8, len(Stransposed)*len(Stransposed[0])*2)
	stbI := 0
	for i := 0; i < len(Stransposed); i++ {
		for j := 0; j < len(Stransposed[i]); j++ {
			stb[stbI] = uint8(Stransposed[i][j] & 0xff)
			stbI++
			stb[stbI] = uint8(Stransposed[i][j] >> 8)
			stbI++
		}
	}

	// Secret key = s || seedA || packed B || encoded Sᵗ || hash of public key
	sk = append(s, seedA...)
	sk = append(sk, b...)
	sk = append(sk, stb...)
	sk = append(sk, pkh...)
	return pk, sk
}

// Generate a KEM returning the cipher-text and shared-secret
func (k *FrodoKEM) Encapsulate(pk []uint8) (ct []uint8, ssEnc []uint8, err error) {
	// Step 1: Validate public key length
	if len(pk) != k.lenSeedA/8+k.d*k.n*k.nBar/8 {
		err = errors.New("incorrect public key length")
		return
	}

	// Step 2: Parse public key components
	seedA := pk[0 : k.lenSeedA/8]
	b := pk[k.lenSeedA/8:]

	// Step 3: Generate random message µ (mu), the plaintext message encoded into the KEM
	mu := make([]uint8, k.lenMu/8)
	k.rng(mu)

	// Step 4: Hash the public key (used as input to KDF)
	pkh := k.shake(pk, k.lenPkh/8)

	// Step 5: Derive seedSE and auxiliary key _k from pkh || µ
	seedSE_k := k.shake(append(pkh, mu...), k.lenSeedSE/8+k.lenK/8)
	seedSE := seedSE_k[0 : k.lenSeedSE/8]
	_k := seedSE_k[k.lenSeedSE/8 : k.lenSeedSE/8+k.lenK/8]

	// Step 6: Sample random noise matrices using seedSE
	r := unpackUint16(k.shake(append([]byte{0x96}, seedSE...), (2*k.mBar*k.n*k.mBar*k.mBar)*k.lenChi/8))

	//sample error matrix S` and E'
	Sprime := k.sampleMatrix(r[0:k.mBar*k.n], k.mBar, k.n)            //S'
	Eprime := k.sampleMatrix(r[k.mBar*k.n:2*k.mBar*k.n], k.mBar, k.n) //E'

	// Step 7: Recompute matrix A from seed
	A := k.gen(seedA)

	// Step 8: Compute B' = S' * A + E' mod q
	Bprime := matrixAddWithMod(matrixMulWithMod2(Sprime, A, k.q), Eprime, k.q)
	c1 := k.pack(Bprime)

	// Step 9: Sample error matrix E'' (used in computing V)
	Eprimeprime := k.sampleMatrix(r[2*k.mBar*k.n:2*k.mBar*k.n+k.mBar*k.nBar], k.mBar, k.nBar)

	// Step 10: Unpack B (from public key)
	B := k.unpack(b, k.n, k.nBar)

	// Step 11: Compute V = S' * B + E'' mod q (approximate shared secret)
	V := matrixAddWithMod(matrixMulWithMod2(Sprime, B, k.q), Eprimeprime, k.q)

	// Step 12: Encode message µ into a matrix and add to V — this is the reconciliation step
	C := uMatrixAdd(V, k.encode(mu), k.q)
	c2 := k.pack(C)

	// Step 13: Final ciphertext is (c1 || c2)
	ct = append(c1, c2...)

	// Step 14: Derive shared secret using a KDF over (ct || _k)
	ssEnc = k.shake(append(ct, _k...), k.lenSS/8)

	return ct, ssEnc, err
}

// Returns the shared secret by using the provided cipher-text and secret-key
func (k *FrodoKEM) Dencapsulate(sk []uint8, ct []uint8) (ssDec []uint8, err error) {
	// Step 1: Verify ciphertext and secret key lengths
	if len(ct) != k.lenCtBytes {
		err = errors.New("incorrect cipher length")
		return
	}
	if len(sk) != k.lenSkBytes {
		err = errors.New("incorrect secret key length")
		return
	}

	// Step 2: Split ciphertext into c1 and c2
	c1, c2 := k.unwrapCt(ct)

	// Step 3: Unwrap secret key into components
	s, seedA, b, Stransposed, pkh := k.unwrapSk(sk)
	S := matrixTranspose(Stransposed)

	// Step 4: Unpack B' and C from ciphertext
	Bprime := k.unpack(c1, k.mBar, k.n)

	//Compute C Unpack c2, n dash and n dash
	C := k.unpack(c2, k.mBar, k.nBar)

	// Step 5: Compute V' = B' * S
	BprimeS := matrixMulWithMod(Bprime, S, k.q)

	// Step 6: Reconstruct µ' = decode(C - B'S)
	M := matrixSubWithMod(C, BprimeS, k.q)
	//decode m
	muPrime := k.decode(M)

	// Step 7: Recompute seedSE' and k' using µ' and pkh
	seedSEprime_kprime := k.shake(append(pkh, muPrime...), k.lenSeedSE/8+k.lenK/8)
	seedSEprime := seedSEprime_kprime[0 : k.lenSeedSE/8]
	kprime := seedSEprime_kprime[k.lenSeedSE/8:]

	// Step 8: Recompute randomness r used in encapsulation
	r := unpackUint16(k.shake(append([]byte{0x96}, seedSEprime...), (2*k.mBar*k.n+k.mBar*k.mBar)*k.lenChi/8))

	// Step 9: Sample matrices S', E', and E''
	Sprime := k.sampleMatrix(r[0:k.mBar*k.n], k.mBar, k.n)
	Eprime := k.sampleMatrix(r[k.mBar*k.n:2*k.mBar*k.n], k.mBar, k.n)
	Eprimeprime := k.sampleMatrix(r[2*k.mBar*k.n:2*k.mBar*k.n+k.mBar*k.nBar], k.mBar, k.nBar)

	// Step 10: Regenerate matrix A and B''
	A := k.gen(seedA)

	//Caluculate B`` From S`A + E`
	Bprimeprime := matrixAddWithMod(matrixMulWithMod2(Sprime, A, k.q), Eprime, k.q)

	// Step 11: Recompute V = S' * B + E''
	B := k.unpack(b, k.n, k.nBar)
	V := matrixAddWithMod(matrixMulWithMod2(Sprime, B, k.q), Eprimeprime, k.q)

	// Step 12: Compute expected C' = V + encode(µ')
	Cprime := uMatrixAdd(V, k.encode(muPrime), k.q)

	// Step 13: Compare received (B', C) with recomputed (B'', C')
	// Constant-time equality to avoid timing attacks
	if constantUint16Equals(Bprime, Bprimeprime)+constantUint16Equals(C, Cprime) == 2 {
		ssDec = k.shake(append(ct, kprime...), k.lenSS/8)

		// Invalid ciphertext: fallback to secret `s` to derive shared secret
	} else {
		ssDec = k.shake(append(ct, s...), k.lenSS/8)
	}
	return ssDec, err
}

// -------------------------------------------------------- OTHER FUNCTIONS USED UNCOMMENTATED --------------------------------------------------------------------------------------------
// Returns the name of this particular FrodoKEM variant, i.e. Frodo640AES
func (k *FrodoKEM) Name() string {
	return k.name
}

// Returns the shared secret (in bytes) this variant generates
func (k *FrodoKEM) SharedSecretLen() int {
	return k.lenSS / 8
}

// Returns the public key length (in bytes) for this variant
func (k *FrodoKEM) PublicKeyLen() int {
	return k.lenPkBytes
}

// Returns the secret key length (in bytes) for this variant
func (k *FrodoKEM) SecretKeyLen() int {
	return k.lenSkBytes
}

// Returns the cipher-text length (in bytes) encapsulating the shared secret for this variant
func (k *FrodoKEM) CipherTextLen() int {
	return k.lenCtBytes
}

// Overrides the default random number generator (crypto/rand)
func (k *FrodoKEM) OverrideRng(newRng func([]byte)) {
	k.rng = newRng
}

func (k *FrodoKEM) unwrapCt(ct []uint8) (c1 []uint8, c2 []uint8) {
	ofs := 0
	size := k.mBar * k.n * k.d / 8
	c1 = ct[ofs:size] // fmt.Println("c1", hex.EncodeToString(c1))
	ofs += size
	size = k.mBar * k.mBar * k.d / 8
	c2 = ct[ofs : ofs+size] // fmt.Println("c2", hex.EncodeToString(c2))
	return
}

func (k *FrodoKEM) unwrapSk(sk []uint8) (s []uint8, seedA []uint8, b []uint8, Stransposed [][]int16, pkh []uint8) {
	ofs := 0
	size := k.lenS / 8
	s = sk[ofs:size] // fmt.Println("s", hex.EncodeToString(s))
	ofs += size
	size = k.lenSeedA / 8
	seedA = sk[ofs : ofs+size] // fmt.Println("seedA", hex.EncodeToString(seedA))
	ofs += size
	size = k.d * k.n * k.nBar / 8
	b = sk[ofs : ofs+size] // fmt.Println("b", hex.EncodeToString(b))

	ofs += size
	size = k.n * k.nBar * 2
	Sbytes := sk[ofs : ofs+size]

	idx := 0
	Stransposed = make([][]int16, k.nBar)
	for i := 0; i < k.nBar; i++ {
		Stransposed[i] = make([]int16, k.n)
		for j := 0; j < k.n; j++ {
			Stransposed[i][j] = int16(Sbytes[idx])
			idx++
			Stransposed[i][j] |= int16(Sbytes[idx]) << 8
			idx++
		}
	}
	// fmt.Println("S^T", Stransposed)

	ofs += size
	size = k.lenPkh / 8
	pkh = sk[ofs : ofs+size] // fmt.Println("pkh", hex.EncodeToString(pkh))

	return
}

func (k *FrodoKEM) sample(r uint16) (e int16) {
	t := int(r >> 1)
	e = 0
	for z := 0; z < len(k.tChi)-1; z++ {
		if t > int(k.tChi[z]) {
			e += 1
		}
	}
	r0 := r % 2
	if r0 == 1 {
		e = -e
	}
	return
}

func (k *FrodoKEM) sampleMatrix(r []uint16, n1 int, n2 int) (E [][]int16) {
	E = make([][]int16, n1)
	for i := 0; i < n1; i++ {
		E[i] = make([]int16, n2)
		for j := 0; j < n2; j++ {
			E[i][j] = k.sample(r[i*n2+j])
		}
	}
	return E
}

// FrodoKEM specification, Algorithm 3: Frodo.Pack
func (k *FrodoKEM) pack(C [][]uint16) (r []byte) {
	rows := len(C)
	cols := len(C[0])
	r = make([]byte, k.d*rows*cols/8)
	var ri = 0
	var packed uint8
	var bits uint8
	for i := 0; i < rows; i++ {
		for j := 0; j < cols; j++ {
			val := C[i][j]
			for b := 0; b < k.d; b++ {
				packed <<= 1
				packed |= uint16BitN(val, k.d-b-1)
				if bits++; bits == 8 {
					r[ri] = packed
					ri++
					packed = 0
					bits = 0
				}
			}
		}
	}
	if bits != 0 {
		r[ri] = packed
	}
	return r
}

// FrodoKEM specification, Algorithm 4: Frodo.Unpack
func (k *FrodoKEM) unpack(b []uint8, n1 int, n2 int) (C [][]uint16) {
	bIdx := 0
	BBit := 7
	C = make([][]uint16, n1)
	for i := 0; i < n1; i++ {
		C[i] = make([]uint16, n2)
		for j := 0; j < n2; j++ {
			var val uint16
			for l := 0; l < k.d; l++ {
				val <<= 1
				val |= uint16(uint8BitN(b[bIdx], BBit))
				if BBit--; BBit < 0 {
					BBit = 7
					bIdx++
				}
			}
			C[i][j] = val
		}
	}
	return
}

// FrodoKEM specification, Algorithm 1
func (k *FrodoKEM) encode(b []uint8) (K [][]uint16) {
	multiplier := int(k.q)
	if multiplier == 0 {
		multiplier = 65536
	}
	if k.b > 0 {
		multiplier /= 2 << (k.b - 1)
	}
	bIdx := 0
	BBit := 0
	K = make([][]uint16, k.mBar)
	for i := 0; i < k.mBar; i++ {
		K[i] = make([]uint16, k.nBar)
		for j := 0; j < k.nBar; j++ {
			var val uint16
			for l := 0; l < k.b; l++ {
				val |= uint16(uint8BitN(b[bIdx], BBit)) << l
				if BBit++; BBit > 7 {
					BBit = 0
					bIdx++
				}
			}
			K[i][j] = val * uint16(multiplier)
		}
	}
	return
}

// FrodoKEM specification, Algorithm 2
func (k *FrodoKEM) decode(K [][]uint16) (b []uint8) {
	b = make([]uint8, k.b*k.mBar*k.nBar/8)
	fixedQ := float64(k.q)
	if k.q == 0 {
		fixedQ = float64(65535)
	}
	twoPowerB := int32(2 << (k.b - 1))
	twoPowerBf := float64(int(2 << (k.b - 1)))
	bIdx := 0
	BBit := 0
	for i := 0; i < k.mBar; i++ {
		for j := 0; j < k.nBar; j++ {
			tmp := uint8(int32(math.Round(float64(K[i][j])*twoPowerBf/fixedQ)) % twoPowerB) //FIXME: please do this better
			for l := 0; l < k.b; l++ {
				if uint8BitN(tmp, l) == 1 {
					b[bIdx] = uint8setBitN(b[bIdx], BBit)
				}
				BBit++
				if BBit == 8 {
					bIdx++
					BBit = 0
				}
			}
		}
	}
	return
}

func (k *FrodoKEM) genSHAKE128(seedA []byte) (A [][]uint16) {
	var c = make([]byte, 2*k.n)
	var tmp = make([]byte, 2+len(seedA))
	copy(tmp[2:], seedA)
	A = make([][]uint16, k.n)
	for i := 0; i < k.n; i++ {
		A[i] = make([]uint16, k.n)
		binary.LittleEndian.PutUint16(tmp[0:], uint16(i))
		sha3.ShakeSum128(c, tmp)
		for j := 0; j < k.n; j++ {
			A[i][j] = binary.LittleEndian.Uint16(c[j*2 : (j+1)*2])
			if k.q != 0 {
				A[i][j] %= k.q
			}
		}
	}
	return
}

func (k *FrodoKEM) genAES128(seedA []byte) (A [][]uint16) {
	A = make([][]uint16, k.n)
	cipher, err := aes.NewCipher(seedA)
	if err != nil {
		panic(err)
	}
	var b = [16]byte{}
	var c = [16]byte{}
	for i := 0; i < k.n; i++ {
		A[i] = make([]uint16, k.n)
		for j := 0; j < k.n; j += 8 {
			binary.LittleEndian.PutUint16(b[0:2], uint16(i))
			binary.LittleEndian.PutUint16(b[2:4], uint16(j))
			cipher.Encrypt(c[:], b[:])
			for l := 0; l < 8; l++ {
				A[i][j+l] = binary.LittleEndian.Uint16(c[l*2 : (l+1)*2])
				if k.q != 0 {
					A[i][j+l] %= k.q
				}
			}

		}
	}
	return
}

// constant time [][]uint16 equals, 1=true, 0=false
func constantUint16Equals(a [][]uint16, b [][]uint16) (ret int) {
	ret = 1
	if len(a) != len(b) {
		panic("Can not compare matrices of different size")
	}
	for i := 0; i < len(a); i++ {
		if len(a[i]) != len(b[i]) {
			panic("Can not compare matrices of different size")
		}
		for j := 0; j < len(a[i]); j++ {
			if a[i][j] != b[i][j] {
				ret = 0
			}
		}
	}
	return
}

func matrixAddWithMod(X [][]uint16, Y [][]int16, q uint16) (R [][]uint16) {
	nrowsx := len(X)
	ncolsx := len(X[0])
	nrowsy := len(Y)
	ncolsy := len(Y[0])
	if nrowsx != nrowsy || ncolsx != ncolsy {
		panic("can't add these matrices")
	}
	R = make([][]uint16, nrowsx)
	for i := 0; i < nrowsx; i++ {
		R[i] = make([]uint16, ncolsx)
		for j := 0; j < ncolsx; j++ {
			R[i][j] = uint16(int16(X[i][j]) + Y[i][j])
			if q != 0 {
				R[i][j] %= q
			}
		}
	}
	return
}

func uMatrixAdd(X [][]uint16, Y [][]uint16, q uint16) (R [][]uint16) {
	nrowsx := len(X)
	ncolsx := len(X[0])
	nrowsy := len(Y)
	ncolsy := len(Y[0])
	if nrowsx != nrowsy || ncolsx != ncolsy {
		panic("can't add these matrices")
	}
	R = make([][]uint16, nrowsx)
	for i := 0; i < nrowsx; i++ {
		R[i] = make([]uint16, ncolsx)
		for j := 0; j < ncolsx; j++ {
			R[i][j] = X[i][j] + Y[i][j]
			if q != 0 {
				R[i][j] %= q
			}
		}
	}
	return
}

func matrixSubWithMod(X [][]uint16, Y [][]uint16, q uint16) (R [][]uint16) {
	nrowsx := len(X)
	ncolsx := len(X[0])
	nrowsy := len(Y)
	ncolsy := len(Y[0])
	if nrowsx != nrowsy || ncolsx != ncolsy {
		panic("can't sub these matrices")
	}
	R = make([][]uint16, nrowsx)
	for i := 0; i < nrowsx; i++ {
		R[i] = make([]uint16, ncolsx)
		for j := 0; j < ncolsx; j++ {
			R[i][j] = X[i][j] - Y[i][j]
			if q != 0 {
				R[i][j] %= q
			}
		}
	}
	return
}

func matrixMulWithMod(X [][]uint16, Y [][]int16, q uint16) (R [][]uint16) {
	nrowsx := len(X)
	ncolsx := len(X[0])
	//nrowsy := len(y)
	ncolsy := len(Y[0])
	R = make([][]uint16, nrowsx)
	for i := 0; i < nrowsx; i++ {
		R[i] = make([]uint16, ncolsy)
		for j := 0; j < ncolsy; j++ {
			var res uint16
			for k := 0; k < ncolsx; k++ {
				res += uint16(int16(X[i][k]) * Y[k][j])
			}
			if q != 0 {
				res %= q
			}
			R[i][j] = res
		}
	}
	return
}

func matrixMulWithMod2(X [][]int16, Y [][]uint16, q uint16) (R [][]uint16) {
	nrowsx := len(X)
	ncolsx := len(X[0])
	//nrowsy := len(y)
	ncolsy := len(Y[0])
	R = make([][]uint16, nrowsx)
	for i := 0; i < nrowsx; i++ {
		R[i] = make([]uint16, ncolsy)
		for j := 0; j < ncolsy; j++ {
			var res uint16
			for k := 0; k < ncolsx; k++ {
				res += uint16(X[i][k] * int16(Y[k][j]))
			}
			if q != 0 {
				res %= q
			}
			R[i][j] = res
		}
	}
	return
}

func matrixTranspose(O [][]int16) (T [][]int16) {
	T = make([][]int16, len(O[0]))
	for x := 0; x < len(T); x++ {
		T[x] = make([]int16, len(O))
		for y := 0; y < len(O); y++ {
			T[x][y] = O[y][x]
		}
	}
	return
}

func unpackUint16(bytes []byte) (r []uint16) {
	r = make([]uint16, len(bytes)/2)
	j := 0
	for i := 0; i+1 < len(bytes); i += 2 {
		r[j] = binary.LittleEndian.Uint16(bytes[i : i+2])
		j++
	}
	return r
}

func uint8setBitN(val uint8, i int) uint8 {
	return val | (1 << i)
}

func uint16BitN(val uint16, i int) uint8 {
	return uint8((val >> i) & 1)
}

func uint8BitN(val uint8, i int) uint8 {
	return (val >> i) & 1
}
