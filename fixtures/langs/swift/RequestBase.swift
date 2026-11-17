import Foundation

public class DownloadRequest2: Request, @unchecked Sendable {
    public struct Options: OptionSet, Sendable {
        public static let createIntermediateDirectories = Options(rawValue: 1 << 0)
        public static let removePreviousFile = Options(rawValue: 1 << 1)

        public let rawValue: Int

        public init(rawValue: Int) {
            self.rawValue = rawValue
        }
    }

    public typealias Destination = @Sendable (_ temporaryURL: URL, _ response: HTTPURLResponse) -> (destinationURL: URL, options: Options)

    @Protected
    private var mutableDownloadState = DownloadState()

    private(set) var resumeData: Data?

    public var fileURL: URL? { mutableDownloadState.fileURL }

    public func cancel(producingResumeData shouldProduceResumeData: Bool) -> Self {
        cancel(producingResumeData: shouldProduceResumeData, completionHandler: nil)
    }

    @discardableResult
    public func cancel(byProducingResumeData completionHandler: @escaping @Sendable (_ data: Data?) -> Void) -> Self {
        cancel(optionallyProducingResumeData: completionHandler)
    }

    @discardableResult
    public func validate(_ validation: @escaping Validation) -> Self {
        let validator: @Sendable () -> Void = { [unowned self] in
            guard error == nil, let response else { return }
            let result = validation(request, response, fileURL)
            if case let .failure(error) = result { self.error = error.asAFError(or: .responseValidationFailed(reason: .customValidationFailed(error: error))) }
        }
        validators.write { $0.append(validator) }
        return self
    }

    private func cancel(optionallyProducingResumeData completionHandler: (@Sendable (_ resumeData: Data?) -> Void)?) -> Self {
        self
    }

    public func response(queue: DispatchQueue = .main, completionHandler: @escaping @Sendable (DownloadResponse<URL?, AFError>) -> Void) -> Self {
        self
    }
}
