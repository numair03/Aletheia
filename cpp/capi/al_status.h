#ifndef ALETHEIA_AL_STATUS_H
#define ALETHEIA_AL_STATUS_H

/*
 * Status codes shared by every Aletheia C API (cpp/capi/ *_c.h).
 *
 * Every exported function returns int32_t: AL_OK (0) on success, a negative
 * code on failure. Functions that answer yes/no (such as proof verification)
 * return 1 or 0 instead of AL_OK and use the negative codes only for errors.
 * No C++ exception ever crosses this boundary.
 */

#define AL_OK 0
#define AL_ERR_ARG (-1)        /* null pointer, out-of-range index, malformed input */
#define AL_ERR_BUFFER (-2)     /* caller's output buffer is too small */
#define AL_ERR_NOT_FOUND (-3)  /* requested item does not exist */
#define AL_ERR_INTERNAL (-100) /* a C++ std::exception (e.g. std::bad_alloc) was caught */
#define AL_ERR_UNKNOWN (-101)  /* a non-std exception was caught */

#ifdef __cplusplus
/* C++-only helper for the implementation files; invisible to C and cgo. */
#include <cstdint>
#include <exception>

namespace aletheia {
namespace capi {

// Runs `body` (which returns an int32_t status) and converts any exception
// into a status code, so that no exception can unwind into a C or Go caller.
template <typename Body>
std::int32_t guarded(Body&& body) noexcept {
    try {
        return body();
    } catch (const std::exception&) {
        return AL_ERR_INTERNAL;
    } catch (...) {
        return AL_ERR_UNKNOWN;
    }
}

} // namespace capi
} // namespace aletheia
#endif /* __cplusplus */

#endif /* ALETHEIA_AL_STATUS_H */
