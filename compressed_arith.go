package sparse

import (
	"github.com/james-bowman/sparse/blas"
	"gonum.org/v1/gonum/mat"
)

// createWorkspace creates a temporary workspace to store the result
// of matrix operations avoiding the issue of mutating operands mid
// operation where they overlap with the receiver
// e.g.
//	result.Mul(result, a)
// createWorkspace will attempt to reuse previously allocated memory
// for the temporary workspace where ever possible to avoid allocating
// memory and placing strain on GC.
func (c *CSR) createWorkspace(dim int, size int, zero bool) (indptr, ind []int, data []float64) {
	indptr = getInts(dim, zero)
	ind = getInts(size, zero)
	data = getFloats(size, zero)
	return
}

// commitWorkspace commits a temporary workspace previously created
// with createWorkspace.  This has the effect of updaing the receiver
// with the values from the temporary workspace and returning the
// memory used by the workspace to the pool for other operations to
// reuse.
func (c *CSR) commitWorkspace(indptr, ind []int, data []float64) {
	c.matrix.Indptr, indptr = indptr, c.matrix.Indptr
	c.matrix.Ind, ind = ind, c.matrix.Ind
	c.matrix.Data, data = data, c.matrix.Data
	putInts(indptr)
	putInts(ind)
	putFloats(data)
}

// MulMatRawVec computes the matrix vector product between lhs and rhs and stores
// the result in out
func MulMatRawVec(lhs *CSR, rhs []float64, out []float64) {
	m, n := lhs.Dims()
	if len(rhs) != n {
		panic(mat.ErrShape)
	}
	if len(out) != m {
		panic(mat.ErrShape)
	}

	blas.Dusmv(false, 1, lhs.RawMatrix(), rhs, 1, out, 1)
}

// Mul takes the matrix product of the supplied matrices a and b and stores the result
// in the receiver.  Some specific optimisations are available for operands of certain
// sparse formats e.g. CSR * CSR uses Gustavson Algorithm (ACM 1978) for fast
// sparse matrix multiplication.
// If the number of columns does not equal the number of rows in b, Mul will panic.
func (c *CSR) Mul(a, b mat.Matrix) {
	ar, ac := a.Dims()
	br, bc := b.Dims()

	if ac != br {
		panic(mat.ErrShape)
	}

	if dia, ok := a.(*DIA); ok {
		// handle case where matrix A is a DIA
		c.mulDIA(dia, b, false)
		return
	}
	if dia, ok := b.(*DIA); ok {
		// handle case where matrix B is a DIA
		c.mulDIA(dia, a, true)
		return
	}
	// TODO: handle cases where both matrices are DIA

	lhs, isLCsr := a.(*CSR)
	rhs, isRCsr := b.(*CSR)
	if isLCsr {
		if isRCsr {
			// handle case where matrix A and B are both CSR
			c.mulCSRCSR(lhs, rhs)
			return
		}
		// handle case where matrix A is CSR
		c.mulCSRMat(lhs, b)
		return
	}
	if isRCsr {
		// handle case where matrix B is CSR
		bt := b.T().(*CSC).ToCSR()
		at := a.T()
		c.mulCSRMat(bt, at)
		*c = *c.T().(*CSC).ToCSR()
		return
	}

	indptr, ind, data := c.createWorkspace(ar+1, 0, false)
	// handle any implementation of mat.Matrix for both matrix A and B
	row := getFloats(ac, false)
	for i := 0; i < ar; i++ {
		// perhaps counter-intuatively, transferring the row elements of the first operand
		// into a slice as part of a separate loop (rather than accessing each element within
		// the main loop (a.At(m, n) * b.At(m, n)) then ranging over them as part of the
		// main loop is about twice as fast.  This is related to lining the data up into CPU
		// cache rather than accessing from RAM.
		// This seems to have interesting implications when using formats with more expensive
		// lookups - placing the more costly format first (and extracting its rows into a
		// slice) appears approximately twice as fast as switching the order of the formats.
		for ci := range row {
			row[ci] = a.At(i, ci)
		}
		for j := 0; j < bc; j++ {
			var v float64
			for ci, e := range row {
				v += e * b.At(ci, j)
			}
			if v != 0 {
				ind = append(ind, j)
				data = append(data, v)
			}
		}
		indptr[i+1] = len(ind)
	}
	putFloats(row)
	c.matrix.I, c.matrix.J = ar, bc
	c.commitWorkspace(indptr, ind, data)
}

// mulCSRMat handles CSR = CSR * mat.Matrix
func (c *CSR) mulCSRMat(lhs *CSR, b mat.Matrix) {
	ar, _ := lhs.Dims()
	_, bc := b.Dims()
	indptr, ind, data := c.createWorkspace(ar+1, 0, false)

	// handle case where matrix A is CSR (matrix B can be any implementation of mat.Matrix)
	for i := 0; i < ar; i++ {
		for j := 0; j < bc; j++ {
			var v float64
			// TODO Consider converting all Sparser args to CSR
			for k := lhs.matrix.Indptr[i]; k < lhs.matrix.Indptr[i+1]; k++ {
				v += lhs.matrix.Data[k] * b.At(lhs.matrix.Ind[k], j)
			}
			if v != 0 {
				ind = append(ind, j)
				data = append(data, v)
			}
		}
		indptr[i+1] = len(ind)
	}
	c.matrix.I, c.matrix.J = ar, bc
	c.commitWorkspace(indptr, ind, data)
}

// mulCSRCSR handles CSR = CSR * CSR using Gustavson Algorithm (ACM 1978)
func (c *CSR) mulCSRCSR(lhs *CSR, rhs *CSR) {
	ar, _ := lhs.Dims()
	_, bc := rhs.Dims()
	indptr, ind, data := c.createWorkspace(ar+1, 0, false)

	spa := NewSPA(bc)

	// rows in C
	for i := 0; i < ar; i++ {
		// each element t in row i of A
		for t := lhs.matrix.Indptr[i]; t < lhs.matrix.Indptr[i+1]; t++ {
			begin := rhs.matrix.Indptr[lhs.matrix.Ind[t]]
			end := rhs.matrix.Indptr[lhs.matrix.Ind[t]+1]
			spa.Scatter(rhs.matrix.Data[begin:end], rhs.matrix.Ind[begin:end], lhs.matrix.Data[t], &ind)
		}
		spa.GatherAndZero(&data, &ind)
		indptr[i+1] = len(ind)
	}
	c.matrix.I, c.matrix.J = ar, bc
	c.commitWorkspace(indptr, ind, data)
}

// mulDIA takes the matrix product of the diagonal matrix dia and an other matrix, other and stores the result
// in the receiver.  This method caters for the specialised case of multiplying by a diagonal matrix where
// significant optimisation is possible due to the sparsity pattern of the matrix.  If trans is true, the method
// will assume that other was the LHS (Left Hand Side) operand and that dia was the RHS.
func (c *CSR) mulDIA(dia *DIA, other mat.Matrix, trans bool) {
	var csMat blas.SparseMatrix
	isCS := false
	var size int

	if csr, ok := other.(*CSR); ok {
		// TODO consider implicitly converting all sparsers to CSR
		// or at least iterating only over the non-zero elements
		csMat = csr.matrix
		isCS = true
		size = len(csMat.Ind)
	}

	rows, cols := other.Dims()
	indptr, ind, data := c.createWorkspace(rows+1, size, true)
	raw := dia.Diagonal()

	if isCS {
		var t int
		for i := 0; i < rows; i++ {
			var v float64
			for k := csMat.Indptr[i]; k < csMat.Indptr[i+1]; k++ {
				var indx int
				if trans {
					indx = csMat.Ind[k]
				} else {
					indx = i
				}
				if indx < len(raw) {
					v = csMat.Data[k] * raw[indx]
					if v != 0 {
						ind[t] = csMat.Ind[k]
						data[t] = v
						t++
					}
				}
			}
			indptr[i+1] = t
		}
	} else {
		for i := 0; i < rows; i++ {
			var v float64
			for k := 0; k < cols; k++ {
				var indx int
				if trans {
					indx = k
				} else {
					indx = i
				}
				if indx < len(raw) {
					v = other.At(i, k) * raw[indx]
					if v != 0 {
						ind = append(ind, k)
						data = append(data, v)
					}
				}
			}
			indptr[i+1] = len(ind)
		}
	}

	c.matrix.I, c.matrix.J = rows, cols
	c.commitWorkspace(indptr, ind, data)
}

// Sub subtracts matrix b from a and stores the result in the receiver.
// If matrices a and b are not the same shape then the method will panic.
func (c *CSR) Sub(a, b mat.Matrix) {
	c.addScaled(a, b, 1, -1)
}

// Add adds matrices a and b together and stores the result in the receiver.
// If matrices a and b are not the same shape then the method will panic.
func (c *CSR) Add(a, b mat.Matrix) {
	c.addScaled(a, b, 1, 1)
}

// addScaled adds matrices a and b scaling them by a and b respectively before hand.
func (c *CSR) addScaled(a mat.Matrix, b mat.Matrix, alpha float64, beta float64) {
	ar, ac := a.Dims()
	br, bc := b.Dims()

	if ar != br || ac != bc {
		panic(mat.ErrShape)
	}

	lCsr, lIsCsr := a.(*CSR)
	rCsr, rIsCsr := b.(*CSR)

	// TODO optimisation for DIA matrices
	if lIsCsr && rIsCsr {
		c.addCSRCSR(lCsr, rCsr, alpha, beta)
		return
	}
	if lIsCsr {
		c.addCSR(lCsr, b, alpha, beta)
		return
	}
	if rIsCsr {
		c.addCSR(rCsr, a, beta, alpha)
		return
	}
	// dumb addition with no sparcity optimisations/savings
	indptr, ind, data := c.createWorkspace(ar+1, 0, false)
	for i := 0; i < ar; i++ {
		for j := 0; j < ac; j++ {
			v := alpha*a.At(i, j) + beta*b.At(i, j)
			//v := a.At(i, j) + b.At(i, j)
			if v != 0 {
				ind = append(ind, j)
				data = append(data, v)
			}
		}
		indptr[i+1] = len(ind)
	}
	c.matrix.I, c.matrix.J = ar, ac
	c.commitWorkspace(indptr, ind, data)
}

// addCSR adds a CSR matrix to any implementation of mat.Matrix and stores the
// result in the receiver.
func (c *CSR) addCSR(csr *CSR, other mat.Matrix, alpha float64, beta float64) {
	ar, ac := csr.Dims()
	indptr, ind, data := c.createWorkspace(ar+1, 0, false)

	spa := NewSPA(ac)
	a := csr.RawMatrix()

	for i := 0; i < ar; i++ {
		begin := csr.matrix.Indptr[i]
		end := csr.matrix.Indptr[i+1]

		if dense, isDense := other.(mat.RawMatrixer); isDense {
			rawOther := dense.RawMatrix()
			r := rawOther.Data[i*rawOther.Stride : i*rawOther.Stride+rawOther.Cols]
			spa.AccumulateDense(r, beta, &ind)
		} else {
			for j := 0; j < ac; j++ {
				v := other.At(i, j)
				if v != 0 {
					spa.ScatterValue(v, j, beta, &ind)
				}
			}
		}
		spa.Scatter(a.Data[begin:end], a.Ind[begin:end], alpha, &ind)
		spa.GatherAndZero(&data, &ind)
		indptr[i+1] = len(ind)
	}
	c.matrix.I, c.matrix.J = ar, ac
	c.commitWorkspace(indptr, ind, data)
}

// addCSRCSR adds 2 CSR matrices together storing the result in the receiver.
// Matrices a and b are scaled by alpha and beta respectively before addition.
// This method is specially optimised to take advantage of the sparsity patterns
// of the 2 CSR matrices.
func (c *CSR) addCSRCSR(lhs *CSR, rhs *CSR, alpha float64, beta float64) {
	ar, ac := lhs.Dims()
	anz, bnz := lhs.NNZ(), rhs.NNZ()
	indptr := make([]int, ar+1)
	ind := make([]int, 0, anz+bnz)
	data := make([]float64, 0, anz+bnz)

	a := lhs.RawMatrix()
	b := rhs.RawMatrix()
	spa := NewSPA(ac)

	var begin, end int
	for i := 0; i < ar; i++ {
		begin, end = a.Indptr[i], a.Indptr[i+1]
		spa.Scatter(a.Data[begin:end], a.Ind[begin:end], alpha, &ind)

		begin, end = b.Indptr[i], b.Indptr[i+1]
		spa.Scatter(b.Data[begin:end], b.Ind[begin:end], beta, &ind)

		spa.GatherAndZero(&data, &ind)
		indptr[i+1] = len(ind)
	}
	c.matrix.I, c.matrix.J = ar, ac
	c.matrix.Indptr = indptr
	c.matrix.Ind = ind
	c.matrix.Data = data
}

// SPA is a SParse Accumulator used to construct the results of sparse
// arithmetic operations in linear time.
type SPA struct {
	// w contains flags for indices containing non-zero values
	w []int

	// x contains all the values in dense representation (including zero values)
	y []float64

	// ls contains the indices of non-zero values
	//ls []int

	// nnz is the Number of Non-Zero elements
	nnz int

	// generation is used to compare values of w to see if they have been set
	// in the current row (generation).  This avoids needing to reset all values
	// during the GatherAndZero operation at the end of
	// construction for each row/column vector.
	generation int
}

// NewSPA creates a new SParse Accumulator of length n.  If accumulating
// rows for a CSR matrix then n should be equal to the number of columns
// in the resulting matrix.
func NewSPA(n int) *SPA {
	return &SPA{
		w: make([]int, n),
		y: make([]float64, n),
	}
}

// ScatterVec accumulates the sparse vector x by multiplying the elements
// by alpha and adding them to the corresponding elements in the SPA
// (SPA += alpha * x)
func (s *SPA) ScatterVec(x *Vector, alpha float64, ind *[]int) {
	s.Scatter(x.data, x.ind, alpha, ind)
}

// Scatter accumulates the sparse vector x by multiplying the elements by
// alpha and adding them to the corresponding elements in the SPA (SPA += alpha * x)
func (s *SPA) Scatter(x []float64, indx []int, alpha float64, ind *[]int) {
	for i, index := range indx {
		s.ScatterValue(x[i], index, alpha, ind)
	}
}

// ScatterValue accumulates a single value by multiplying the value by alpha
// and adding it to the corresponding element in the SPA (SPA += alpha * x)
func (s *SPA) ScatterValue(val float64, index int, alpha float64, ind *[]int) {
	if s.w[index] < s.generation+1 {
		s.w[index] = s.generation + 1
		*ind = append(*ind, index)
		s.y[index] = alpha * val
	} else {
		s.y[index] += alpha * val
	}
}

// AccumulateDense accumulates the dense vector x by multiplying the non-zero elements
// by alpha and adding them to the corresponding elements in the SPA (SPA += alpha * x)
// This is the dense version of the Scatter method for sparse vectors.
func (s *SPA) AccumulateDense(x []float64, alpha float64, ind *[]int) {
	for i, val := range x {
		if val != 0 {
			s.ScatterValue(val, i, alpha, ind)
		}
	}
}

// Gather gathers the non-zero values from the SPA and appends them to
// end of the supplied sparse vector.
func (s SPA) Gather(data *[]float64, ind *[]int) {
	for _, index := range (*ind)[s.nnz:] {
		*data = append(*data, s.y[index])
		//y[index] = 0
	}
}

// GatherAndZero gathers the non-zero values from the SPA and appends them
// to the end of the supplied sparse vector.  The SPA is also zeroed
// ready to start accumulating the next row/column vector.
func (s *SPA) GatherAndZero(data *[]float64, ind *[]int) {
	s.Gather(data, ind)

	s.nnz = len(*ind)
	s.generation++
}
