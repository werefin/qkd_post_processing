#include "textflag.h"

// func clmul64(a, b uint64) (hi, lo uint64)
TEXT ·clmul64(SB), NOSPLIT, $0-32
	MOVQ  a+0(FP), AX
	MOVQ  b+8(FP), CX
	MOVQ  AX, X0
	MOVQ  CX, X1
	PCLMULQDQ $0x00, X0, X1 // X1 = 128-bit carry-less product of the two low qwords
	PEXTRQ $1, X1, AX
	MOVQ  AX, hi+16(FP)
	MOVQ  X1, AX
	MOVQ  AX, lo+24(FP)
	RET

// func hasCLMUL() bool
TEXT ·hasCLMUL(SB), NOSPLIT, $0-1
	MOVL $1, AX
	MOVL $0, CX
	CPUID
	ANDL $2, CX // PCLMULQDQ is ECX bit 1 of CPUID.01H
	SETNE ret+0(FP)
	RET
