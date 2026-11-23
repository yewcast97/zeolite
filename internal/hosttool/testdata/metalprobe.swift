// metalprobe is a test's Host Tool: it computes on this Mac's GPU, and
// says so. With --archive it keeps what the compiler built in a Metal
// binary archive, and with --save writes that archive to a path. With
// --include it asks the shader compiler to read a file, and prints what
// the compiler says of it.
import Foundation
import Metal

let args = CommandLine.arguments
func arg(_ name: String) -> String? {
    guard let i = args.firstIndex(of: name), i + 1 < args.count else { return nil }
    return args[i + 1]
}
guard let dev = MTLCreateSystemDefaultDevice() else { print("NO DEVICE"); exit(2) }
print("device: \(dev.name)")

if let path = arg("--include") {
    // Does the compiler read a file the caller names?
    let src = "#include <metal_stdlib>\n#include \"\(path)\"\nusing namespace metal;\nkernel void k(device float *a [[buffer(0)]], uint id [[thread_position_in_grid]]) { a[id] = 1.0; }\n"
    do {
        _ = try dev.makeLibrary(source: src, options: nil)
        print("INCLUDE COMPILED")
    } catch {
        print("INCLUDE ERROR: \(error.localizedDescription.prefix(600))")
    }
    exit(0)
}

let src = """
#include <metal_stdlib>
using namespace metal;
kernel void twice(device float *a [[buffer(0)]], uint id [[thread_position_in_grid]]) { a[id] = a[id] * 2.0 + 1.0; }
"""
do {
    let lib = try dev.makeLibrary(source: src, options: nil)
    let desc = MTLComputePipelineDescriptor()
    desc.computeFunction = lib.makeFunction(name: "twice")!
    var archive: MTLBinaryArchive? = nil
    if args.contains("--archive") || arg("--load") != nil {
        let ad = MTLBinaryArchiveDescriptor()
        if let p = arg("--load") { ad.url = URL(fileURLWithPath: p) }
        archive = try dev.makeBinaryArchive(descriptor: ad)
        print("archive made")
        try archive!.addComputePipelineFunctions(descriptor: desc)
        print("archive filled")
        desc.binaryArchives = [archive!]
    }
    let pso = try dev.makeComputePipelineState(descriptor: desc, options: [], reflection: nil)
    let n = 4096
    let buf = dev.makeBuffer(length: n * 4, options: .storageModeShared)!
    let p = buf.contents().bindMemory(to: Float.self, capacity: n)
    for i in 0..<n { p[i] = Float(i) }
    let cb = dev.makeCommandQueue()!.makeCommandBuffer()!
    let enc = cb.makeComputeCommandEncoder()!
    enc.setComputePipelineState(pso)
    enc.setBuffer(buf, offset: 0, index: 0)
    enc.dispatchThreads(MTLSize(width: n, height: 1, depth: 1), threadsPerThreadgroup: MTLSize(width: min(n, pso.maxTotalThreadsPerThreadgroup), height: 1, depth: 1))
    enc.endEncoding()
    cb.commit()
    cb.waitUntilCompleted()
    var ok = cb.status == .completed
    for i in 0..<n where p[i] != Float(i) * 2 + 1 { ok = false }
    print(ok ? "GPU OK" : "GPU WRONG (status \(cb.status.rawValue))")
    if let a = archive, let path = arg("--save") {
        try a.serialize(to: URL(fileURLWithPath: path))
        print("archive saved")
    }
} catch {
    print("ERROR: \(error.localizedDescription.prefix(600))")
    exit(1)
}
