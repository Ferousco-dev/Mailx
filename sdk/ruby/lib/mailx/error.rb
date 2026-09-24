module MailX
  class APIError < StandardError
    attr_reader :status, :type, :code, :request_id, :retry_after

    def initialize(status:, type:, code:, message:, request_id: nil, retry_after: nil)
      super(message)
      @status = status
      @type = type
      @code = code
      @request_id = request_id
      @retry_after = retry_after
    end
  end
end
