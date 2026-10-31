# A Shiny app whose memory grows when you ask it to, for watching a copy go
# over the manifest's max-memory.
#
# The page shows which R process is serving it and how much it holds. "Add
# 1 GB" adds 1 GB at once; "Grow until stopped" adds 100 MB per second.
# Nothing grows on start. Once the copy holds more than the manifest's
# max-memory, shinyctl kills it: the page drops, reloads on the warm copy,
# and starts again from 0 MB. MAX_MB (set in the manifest's env) is a safety
# stop, so the app never takes the whole machine if max-memory is raised or
# removed. The memory belongs to the R process, not to the browser tab:
# closing the tab stops the growth but keeps what is held, until a restart.

library(shiny)

# Top-level code runs once per R process, not once per browser session, so
# this is when the current process started.
started_at <- Sys.time()
max_mb <- as.integer(Sys.getenv("MAX_MB", "1024"))
step_mb <- 100

# Process-level, not per session: every tab on this copy sees one total.
heap <- new.env()
heap$chunks <- list()
heap$mb <- 0

# allocate adds mb MB of touched memory: runif() writes every element, so
# the memory is really used, unlike a compact ALTREP sequence like 1:n,
# which takes no memory at all.
allocate <- function(mb) {
  if (heap$mb + mb > max_mb) return(FALSE)
  n <- mb * 1024^2 / 8
  x <- runif(n)
  heap$chunks[[length(heap$chunks) + 1]] <- x
  heap$mb <- heap$mb + mb
  message(sprintf("holding %d MB", heap$mb))
  TRUE
}

ui <- fluidPage(
  # Reloads the page once the app answers again after its R process died,
  # instead of leaving it greyed out. A reload, not Shiny's own reconnect:
  # with shiny 1.14, a page reconnected to a new process keeps showing its
  # old outputs, and its download links stop working. Skipped in the first
  # 3 seconds after a load, so an app that fails at start isn't reloaded
  # over and over.
  tags$script(HTML(
    "$(document).on('shiny:disconnected', function poll() {
       if (performance.now() < 3000) return;
       fetch(location.href, {cache: 'no-store'})
         .then(r => r.ok ? location.reload() : Promise.reject())
         .catch(() => setTimeout(poll, 500));
     });"
  )),
  titlePanel("shinyctl memory example"),
  p(textOutput("process")),
  h4(textOutput("held")),
  actionButton("add1g", "Add 1 GB"),
  actionButton("grow", "Grow until stopped"),
  p("The memory grows until this copy holds more than the manifest's",
    "max-memory. Then shinyctl kills it and the page reloads on the warm",
    "copy, holding 0 MB. MAX_MB, set in the manifest's env, is a safety stop.",
    "What is held belongs to this R process: closing the tab stops the",
    "growth but keeps what is held.")
)

server <- function(input, output, session) {
  output$process <- renderText(sprintf(
    "Served by R process %d, started at %s.",
    Sys.getpid(), format(started_at, "%H:%M:%S")
  ))

  # Every tab shows the copy's real total, so it refreshes on its own.
  output$held <- renderText({
    invalidateLater(1000)
    sprintf("Holding %d MB.", heap$mb)
  })

  # An allocation that fails ("cannot allocate vector") must not end the
  # session, and must not also report the ceiling.
  add <- function(mb) {
    ok <- tryCatch(allocate(mb), error = function(e) {
      showNotification(conditionMessage(e), type = "error")
      NA
    })
    if (is.na(ok)) return(FALSE)
    if (!ok) showNotification(sprintf("MAX_MB (%d) reached", max_mb))
    ok
  }

  observeEvent(input$add1g, {
    for (i in 1:10) if (!add(step_mb)) break
  })

  # Owned by the session, so closing the tab stops it.
  growing <- reactiveVal(FALSE)
  observeEvent(input$grow, {
    growing(!growing())
    updateActionButton(session, "grow",
                       label = if (growing()) "Stop" else "Grow until stopped")
  })
  observe({
    if (!growing()) return()
    if (add(step_mb)) {
      invalidateLater(1000)
    } else {
      growing(FALSE)
      updateActionButton(session, "grow", label = "Grow until stopped")
    }
  })
}

shinyApp(ui, server)
