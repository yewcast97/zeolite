// The GPU Shim: a library loaded into a Host Tool that is given the GPU.
// It answers, in the tool's own process, two questions whose answer the
// system would fetch from a service the Host Tool Wall refuses.
//
// Where the user's cache folder is. The system asks its directory
// service, which also knows the user's name; without an answer Metal's
// binary archives end the program. The answer here is a folder of the
// Host Tool Scratch, which the app names in ZEOLITE_USER_CACHE_DIR: what
// Metal keeps of the shaders it compiled stays there.
//
// Whether a piece of code is signed as it says. The system asks its
// trust service, which fetches from the network what a certificate
// names, for whoever asks: a way out of this Mac that the Wall does not
// leave open. Without an answer the hardware's video encoders are not
// found. The answer here is yes. A Host Tool is trusted with nothing, so
// what it believes of a signature guards nothing of this Mac's.
//
// Every other call goes where it went. A program signed so that nothing
// is loaded into it does not load the shim, and is as it was without.
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static size_t cache_folder(int name, char *buf, size_t len) {
    const char *dir = name == _CS_DARWIN_USER_CACHE_DIR ? getenv("ZEOLITE_USER_CACHE_DIR") : NULL;
    if (dir == NULL) return confstr(name, buf, len);
    if (buf != NULL && len > 0) strlcpy(buf, dir, len);
    return strlen(dir) + 1;
}

static OSStatus signed_as_said(SecStaticCodeRef code, SecCSFlags flags, SecRequirementRef requirement) {
    return errSecSuccess;
}

static OSStatus signed_as_said_with_errors(SecStaticCodeRef code, SecCSFlags flags, SecRequirementRef requirement, CFErrorRef *errors) {
    if (errors != NULL) *errors = NULL;
    return errSecSuccess;
}

__attribute__((used, section("__DATA,__interpose")))
static const struct { const void *answer, *asked; } answers[] = {
    {(const void *)cache_folder, (const void *)confstr},
    {(const void *)signed_as_said, (const void *)SecStaticCodeCheckValidity},
    {(const void *)signed_as_said_with_errors, (const void *)SecStaticCodeCheckValidityWithErrors},
};
