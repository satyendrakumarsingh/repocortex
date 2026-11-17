import Combine
import Foundation

public struct DataStreamPublisher<Value: Sendable>: Publisher {
    public typealias Output = DataStreamRequest.Stream<Value, Never>
    public typealias Failure = Never

    private typealias Handler = (@escaping DataStreamRequest.Handler<Value, Never>) -> DataStreamRequest

    private let request: DataStreamRequest
    private let streamHandler: Handler

    public init(_ request: DataStreamRequest, queue: DispatchQueue, handler: @escaping Handler) {
        self.request = request
        streamHandler = handler
    }

    public func receive<S: Subscriber>(subscriber: S) where S.Input == Output {
        subscriber.receive(subscription: Inner(request: request, streamHandler: streamHandler, downstream: subscriber))
    }

    private final class Inner<Downstream: Subscriber>: Subscription, Sendable
        where Downstream.Input == Output {
        typealias Failure = Downstream.Failure

        private let downstream: Protected<Downstream?>
        private var request: DataStreamRequest
        private let streamHandler: Handler

        init(request: DataStreamRequest, streamHandler: @escaping Handler, downstream: Downstream) {
            self.request = request
            self.streamHandler = streamHandler
            self.downstream = Protected(downstream)
        }

        func request(_ demand: Subscribers.Demand) {
            assert(demand > 0)
            guard let downstream = downstream.read({ $0 }) else { return }
            self.request = streamHandler { stream in
                _ = downstream.receive(stream)
            }
            request.resume()
        }

        func cancel() {
            downstream.write(nil)
            request.cancel()
        }
    }
}

public struct DownloadPublisher<Value: Sendable>: Publisher {
    public typealias Output = Int
    public typealias Failure = Never

    private typealias Handler = (@escaping (_ response: Int) -> Void) -> Int

    private final class Cache {
        private let storage = NSCache<NSString, NSData>()

        func lookup(_ key: String) -> Data? {
            storage.object(forKey: key as NSString) as Data?
        }

        func store(_ data: Data, for key: String) {
            storage.setObject(data as NSData, forKey: key as NSString)
        }
    }

    private final class Counter<Tag> {
        private var value = 0

        func bump() -> Int {
            value += 1
            return value
        }
    }

    public func receive<S: Subscriber>(subscriber: S) where S.Input == Output {}
}
