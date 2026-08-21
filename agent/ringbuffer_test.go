package agent

import "testing"

func TestRingBuffer_ReadFromOffset(t *testing.T) {
	rb := NewRingBuffer(1024)
	rb.Write([]byte("hello "))
	rb.Write([]byte("world"))

	data, next, trunc := rb.ReadFrom(0, 64)
	if string(data) != "hello world" || trunc {
		t.Fatalf("data=%q next=%d trunc=%v", data, next, trunc)
	}
	if next != 11 {
		t.Fatalf("next = %d, want 11", next)
	}

	data2, next2, _ := rb.ReadFrom(next, 64)
	if len(data2) != 0 || next2 != next {
		t.Fatalf("expected no new data, got %q next=%d", data2, next2)
	}
}

func TestRingBuffer_DropsOldDataAndReportsTruncation(t *testing.T) {
	rb := NewRingBuffer(4) // tiny capacity to force wraparound
	rb.Write([]byte("ab"))
	rb.Write([]byte("cd"))
	rb.Write([]byte("ef")) // "ab" is now dropped; buffer holds "cdef"

	data, next, trunc := rb.ReadFrom(0, 64)
	if !trunc {
		t.Fatalf("expected truncated_before=true")
	}
	if string(data) != "cdef" {
		t.Fatalf("data = %q", data)
	}
	if next != 6 {
		t.Fatalf("next = %d, want 6", next)
	}
}

func TestRingBuffer_MaxBytesLimitsRead(t *testing.T) {
	rb := NewRingBuffer(1024)
	rb.Write([]byte("0123456789"))
	data, next, _ := rb.ReadFrom(0, 4)
	if string(data) != "0123" || next != 4 {
		t.Fatalf("data=%q next=%d", data, next)
	}
}
