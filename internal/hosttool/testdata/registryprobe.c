// registryprobe asks this Mac's hardware registry what a Host Tool may
// not learn of it, and prints a line for each: its name, and "ok" or
// "refused". The tests build it under the Wall and run it there, and
// once with no Wall, where every line is "ok".
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>
#include <stdio.h>

static void say(const char *name, int ok) { printf("%s %s\n", name, ok ? "ok" : "refused"); }

static int has(io_registry_entry_t entry, CFStringRef property) {
    if (!entry) return 0;
    CFTypeRef value = IORegistryEntryCreateCFProperty(entry, property, kCFAllocatorDefault, 0);
    if (!value) return 0;
    CFRelease(value);
    return 1;
}

int main(void) {
    // The whole registry, from its root.
    int walked = 0;
    io_registry_entry_t root = IORegistryGetRootEntry(kIOMainPortDefault);
    io_iterator_t all = 0;
    if (root && IORegistryEntryCreateIterator(root, kIOServicePlane, kIORegistryIterateRecursively, &all) == KERN_SUCCESS && all) {
        io_registry_entry_t entry;
        while ((entry = IOIteratorNext(all))) { walked++; IOObjectRelease(entry); }
    }
    say("walk-the-registry", walked > 0);

    // One device, found by its class: the Mac itself, which every Mac has.
    io_service_t device = IOServiceGetMatchingService(kIOMainPortDefault, IOServiceMatching("IOPlatformExpertDevice"));
    say("find-a-device-by-its-class", device != 0);

    io_name_t name = "";
    say("name-a-device", device && IORegistryEntryGetName(device, name) == KERN_SUCCESS && name[0]);
    io_string_t path = "";
    say("path-of-a-device", device && IORegistryEntryGetPath(device, kIOServicePlane, path) == KERN_SUCCESS && path[0]);
    io_name_t class = "";
    say("class-of-a-device", device && IOObjectGetClass(device, class) == KERN_SUCCESS && class[0]);
    uint64_t number = 0;
    say("number-of-a-device", device && IORegistryEntryGetRegistryEntryID(device, &number) == KERN_SUCCESS && number);
    io_registry_entry_t parent = 0;
    say("parent-of-a-device", device && IORegistryEntryGetParentEntry(device, kIOServicePlane, &parent) == KERN_SUCCESS && parent);
    io_iterator_t children = 0;
    say("children-of-a-device", device && IORegistryEntryGetChildIterator(device, kIOServicePlane, &children) == KERN_SUCCESS && children && IOIteratorNext(children));

    // The class of a driver is a property of it too: asked of every
    // device but the graphics device and its surfaces, which are a
    // tool's with the GPU.
    int classes = 0;
    io_iterator_t services = 0;
    if (IOServiceGetMatchingServices(kIOMainPortDefault, IOServiceMatching("IOService"), &services) == KERN_SUCCESS && services) {
        io_service_t service;
        while ((service = IOIteratorNext(services))) {
            if (!IOObjectConformsTo(service, "IOAccelerator") && !IOObjectConformsTo(service, "IOSurfaceRoot") && has(service, CFSTR("IOClass"))) classes++;
            IOObjectRelease(service);
        }
    }
    say("class-of-a-device-by-a-property", classes > 0);
    say("serial-number-of-this-mac", has(device, CFSTR("IOPlatformSerialNumber")));
    return 0;
}
