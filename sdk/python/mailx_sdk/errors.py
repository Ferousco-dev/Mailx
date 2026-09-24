class MailXError(Exception):
    def __init__(self, status: int, error_type: str, code: str, message: str, request_id: str = None, retry_after: float = None):
        super().__init__(message)
        self.status = status
        self.type = error_type
        self.code = code
        self.message = message
        self.request_id = request_id
        self.retry_after = retry_after

    def __repr__(self):
        return f"MailXError(status={self.status}, type={self.type!r}, code={self.code!r}, message={self.message!r})"
