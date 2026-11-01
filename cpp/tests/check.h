#pragma once

// Minimal test support shared by every C++ test executable (cpp/tests/test_*.cpp).
// Each test file is its own program: it calls CHECK* inside plain functions and
// returns aletheia_test::finish(...) from main(). A failed check is reported
// and counted but does not stop the program, so one run shows every failure.

#include <cstdio>

namespace aletheia_test {

inline int& checks() { static int n = 0; return n; }
inline int& failures() { static int n = 0; return n; }

inline void record(bool ok, const char* file, int line, const char* expr) {
    ++checks();
    if (!ok) {
        ++failures();
        std::fprintf(stderr, "%s:%d: CHECK failed: %s\n", file, line, expr);
    }
}

// Print a one-line summary; the return value is main()'s exit code.
inline int finish(const char* suite) {
    std::printf("%s: %d checks, %d failed\n", suite, checks(), failures());
    return failures() == 0 ? 0 : 1;
}

} // namespace aletheia_test

#define CHECK(cond) ::aletheia_test::record(static_cast<bool>(cond), __FILE__, __LINE__, #cond)

#define CHECK_EQ(a, b) ::aletheia_test::record((a) == (b), __FILE__, __LINE__, #a " == " #b)

// Passes only if evaluating `expr` throws an exception of type `ex`.
#define CHECK_THROWS(expr, ex)                                                     \
    do {                                                                           \
        bool thrown_ = false;                                                      \
        try { (void)(expr); } catch (const ex&) { thrown_ = true; } catch (...) {} \
        ::aletheia_test::record(thrown_, __FILE__, __LINE__, #expr " throws " #ex); \
    } while (0)
