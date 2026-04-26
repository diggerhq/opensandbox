use thiserror::Error;

pub type Result<T> = std::result::Result<T, Error>;

// The WebSocket and HTTP error types are >100 bytes, which would push every
// `Result<T>` past the `result_large_err` threshold. Box them so the SDK's
// happy-path `Result` stays small.
#[derive(Debug, Error)]
pub enum Error {
    #[error("HTTP error: {0}")]
    Http(#[from] Box<reqwest::Error>),

    #[error("WebSocket error: {0}")]
    WebSocket(#[from] Box<tokio_tungstenite::tungstenite::Error>),

    #[error("URL parse error: {0}")]
    Url(#[from] url::ParseError),

    #[error("JSON error: {0}")]
    Json(#[from] Box<serde_json::Error>),

    #[error("API returned status {status}: {body}")]
    Api { status: u16, body: String },

    #[error("Build failed: {0}")]
    BuildFailed(String),

    #[error("{0}")]
    Other(String),
}

impl Error {
    pub(crate) fn other(msg: impl Into<String>) -> Self {
        Self::Other(msg.into())
    }
}

// Convenience `From` impls so `?` works with the un-boxed library error types.
impl From<reqwest::Error> for Error {
    fn from(e: reqwest::Error) -> Self {
        Error::Http(Box::new(e))
    }
}

impl From<tokio_tungstenite::tungstenite::Error> for Error {
    fn from(e: tokio_tungstenite::tungstenite::Error) -> Self {
        Error::WebSocket(Box::new(e))
    }
}

impl From<serde_json::Error> for Error {
    fn from(e: serde_json::Error) -> Self {
        Error::Json(Box::new(e))
    }
}
